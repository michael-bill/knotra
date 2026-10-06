package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func TestArtifactPublicationNeverReplacesExistingBytes(t *testing.T) {
	for _, mode := range []string{"identical", "corrupt", "symlink", "directory"} {
		t.Run(mode, func(t *testing.T) {
			storage := Artifacts{Root: filepath.Join(t.TempDir(), "new", "artifacts")}
			data := []byte("original")
			artifact, err := storage.Write("test.txt", "text/plain", data, nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(storage.Root, artifact.SHA256)
			switch mode {
			case "corrupt":
				if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mode == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					outside := filepath.Join(t.TempDir(), "outside")
					if err := os.WriteFile(outside, data, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						// Windows symlink creation needs developer mode or privilege.
						t.Skipf("filesystem cannot create test symlink: %v", err)
					}
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = storage.Write("again.txt", "text/plain", data, nil)
			if (mode == "identical" && err != nil) || (mode != "identical" && err == nil) {
				t.Fatalf("publication %s error=%v", mode, err)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatalf("existing content was replaced: %v", statErr)
			}
			if mode == "corrupt" {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != "tampered" {
					t.Fatal("silently repaired damaged evidence", err)
				}
			}
		})
	}
}

// Independent writer processes share only the filesystem. They exit without
// closing storage handles; a fresh reader must recover complete immutable files.
func TestSharedArtifactAndOutcomePublicationSurvivesWriterExit(t *testing.T) {
	if host := os.Getenv("KNOTRA_SHARED_STORAGE_WRITER"); host != "" {
		directory := os.Getenv("KNOTRA_SHARED_STORAGE_DIRECTORY")
		if err := os.WriteFile(filepath.Join(directory, host+".ready"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := os.Stat(filepath.Join(directory, "start")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		data := []byte(strings.Repeat("shared artifact\n", 8192))
		artifact, err := (Artifacts{Root: filepath.Join(directory, "artifacts")}).Write(host+".txt", "text/plain", data, nil)
		if err != nil {
			t.Fatal(err)
		}
		files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
		if err != nil {
			t.Fatal(err)
		}
		outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, PlanID: "frozen-plan",
			Ownership:   execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: host, Number: 1}, WorkerID: host, Generation: 1},
			CompletedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
			Outputs:     contract.Values{"file": {Artifacts: []contract.Artifact{artifact}}},
			Artifacts:   []execution.ArtifactReference{{ID: artifact.ID, SHA256: artifact.SHA256, Size: artifact.Size}}}
		key, err := files.Put(outcome)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			Key     string
			Outcome execution.Outcome
		}{key, outcome}); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // skip Close and test cleanup, as in an abruptly exiting writer
	}
	directory := t.TempDir()
	var output [2]bytes.Buffer
	var done [2]chan error
	for index := range 2 {
		host := fmt.Sprintf("writer-%d", index)
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSharedArtifactAndOutcomePublicationSurvivesWriterExit$", "-test.timeout=30s")
		command.Env = append(os.Environ(), "KNOTRA_SHARED_STORAGE_WRITER="+host, "KNOTRA_SHARED_STORAGE_DIRECTORY="+directory)
		command.Stdout, command.Stderr = &output[index], &output[index]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill() })
		done[index] = make(chan error, 1)
		go func() { done[index] <- command.Wait() }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for index := range 2 {
		for {
			if _, err := os.Stat(filepath.Join(directory, fmt.Sprintf("writer-%d.ready", index))); err == nil {
				break
			}
			select {
			case err := <-done[index]:
				t.Fatalf("writer exited before barrier: %v %s", err, output[index].String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("writers did not reach shared publication barrier")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "start"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	var firstHash string
	for index := range 2 {
		if err := <-done[index]; err != nil {
			t.Fatalf("shared writer: %v %s", err, output[index].String())
		}
		var report struct {
			Key     string
			Outcome execution.Outcome
		}
		if err := json.Unmarshal(output[index].Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		got, err := files.Get(report.Key)
		if err != nil || !reflect.DeepEqual(got, report.Outcome) {
			t.Fatalf("shared evidence changed: %+v error=%v", got, err)
		}
		artifact := got.Outputs["file"].Artifacts[0]
		root, err := os.OpenRoot(filepath.Join(directory, "artifacts"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := readArtifactFile(root, artifact)
		closeErr := root.Close()
		if err != nil || closeErr != nil || string(data) != strings.Repeat("shared artifact\n", 8192) {
			t.Fatalf("shared artifact changed: read=%v close=%v", err, closeErr)
		}
		if index == 0 {
			firstHash = artifact.SHA256
		} else if artifact.SHA256 != firstHash {
			t.Fatal("identical shared artifacts have different content addresses")
		}
	}
	entries, err := os.ReadDir(filepath.Join(directory, "artifacts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("shared publication retained temporary or duplicate bytes: files=%d error=%v", len(entries), err)
	}
}
