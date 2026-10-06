package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/michael-bill/knotra/internal/contract"
)

func testSetup(t *testing.T) *Setup {
	t.Helper()
	return &Setup{Config: Config{Dir: t.TempDir(), Provider: "openai", Model: "fixture-model", PostgresPort: 25432}, Getenv: func(string) string { return "fixture-key" }}
}

func TestSetupPreservesEditedFilesAndStableIdentity(t *testing.T) {
	s := testSetup(t)
	first, err := s.WriteOnce()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.Config.Dir, "examples", "hello", "pipeline.yaml")
	edited := []byte("user changes")
	if err = os.WriteFile(file, edited, 0600); err != nil {
		t.Fatal(err)
	}
	second, err := s.WriteOnce()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(actual, edited) || first.OperationID != second.OperationID || first.Password != second.Password {
		t.Fatal("restart replaced content or identity")
	}
	s.Config.Provider = "anthropic"
	if _, err = s.WriteOnce(); err == nil {
		t.Fatal("different setup reused the same database and profile")
	}
}

func TestBundledProfilesAndPackagesCompileForEveryProvider(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			s := testSetup(t)
			s.Config.Provider = provider
			if _, err := s.WriteOnce(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(s.Config.Dir, "profile.json"))
			if err != nil {
				t.Fatal(err)
			}
			profile, diagnostics := contract.ParseProfile(data)
			if contract.HasErrors(diagnostics) {
				t.Fatal(diagnostics)
			}
			for _, entry := range []string{"hello/pipeline.yaml", "research-dossier/review.yaml", "publication/pipeline.yaml", "tic-tac-toe/pipeline.yaml"} {
				pkg, err := contract.LoadPackage(filepath.Join(s.Config.Dir, "examples", filepath.FromSlash(entry)))
				if err != nil {
					t.Fatal(err)
				}
				if _, diagnostics := contract.Compile(pkg, &profile); contract.HasErrors(diagnostics) {
					t.Fatalf("%s: %v", entry, diagnostics)
				}
			}
			if strings.Contains(string(data), "fixture-key") {
				t.Fatal("credential value was written into profile")
			}
		})
	}
}

func TestDoctorChecksCredentialsAndHelperArchitectureBeforeStartup(t *testing.T) {
	s := testSetup(t)
	helper := make([]byte, 20)
	copy(helper, "\x7fELF")
	helper[18] = 183
	s.Config.HelperPath = filepath.Join(s.Config.Dir, "helper")
	if err := os.WriteFile(s.Config.HelperPath, helper, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Command = func(_ context.Context, command string, args, _ []string) ([]byte, error) {
		calls++
		if strings.Join(args, " ") == "info --format {{.OSType}} {{.Architecture}}" {
			return []byte("linux aarch64\n"), nil
		}
		if strings.Join(args, " ") == "compose version" {
			return []byte("Docker Compose"), nil
		}
		return nil, fmt.Errorf("unexpected mutation: %s %v", command, args)
	}
	s.Getenv = func(string) string { return "" }
	if err := s.Doctor(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing key accepted: %v", err)
	}
	if calls != 2 {
		t.Fatal("doctor started infrastructure")
	}
	s.Getenv = func(string) string { return "fixture-key" }
	if err := s.Doctor(context.Background()); err != nil {
		t.Fatal(err)
	}
	helper[18] = 62
	if err := os.WriteFile(s.Config.HelperPath, helper, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Doctor(context.Background()); err == nil {
		t.Fatal("wrong helper architecture accepted")
	}
}

func TestDoctorRejectsWindowsContainersBeforeStartup(t *testing.T) {
	s := testSetup(t)
	s.Command = func(_ context.Context, executable string, args, _ []string) ([]byte, error) {
		if executable != "docker" || strings.Join(args, " ") != "info --format {{.OSType}} {{.Architecture}}" {
			t.Fatalf("unexpected infrastructure command: %s %v", executable, args)
		}
		return []byte("windows x86_64\n"), nil
	}
	if err := s.Doctor(context.Background()); err == nil || !strings.Contains(err.Error(), "Linux containers") {
		t.Fatalf("Windows container daemon accepted: %v", err)
	}
}

func TestInfrastructureStartupUsesSavedCredentialsAndPersistentVolumes(t *testing.T) {
	s := testSetup(t)
	s.compose = "docker"
	var projects []string
	var passwords []string
	s.Command = func(_ context.Context, _ string, args, env []string) ([]byte, error) {
		if strings.Join(args[:2], " ") == "compose --project-name" {
			projects = append(projects, args[2])
			for _, value := range env {
				if strings.HasPrefix(value, "KNOTRA_DEV_DB_PASSWORD=") {
					passwords = append(passwords, value)
				}
			}
			if strings.Contains(strings.Join(args, " "), "down") {
				t.Fatal("startup deletes infrastructure")
			}
		}
		return nil, nil
	}
	var previousDSN, previousID string
	for range 2 {
		dsn, id, err := s.StartInfrastructure(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if previousDSN != "" && (dsn != previousDSN || id != previousID) {
			t.Fatal("startup did not preserve saved state")
		}
		previousDSN, previousID = dsn, id
	}
	if len(projects) != 2 || projects[0] != projects[1] || passwords[0] != passwords[1] {
		t.Fatal("infrastructure identity drifted")
	}
	var saved settings
	data, err := os.ReadFile(filepath.Join(s.Config.Dir, "settings.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Password == "" {
		t.Fatal("settings missing")
	}
}

func TestConcurrentInitializationUsesOnePersistentIdentity(t *testing.T) {
	dir := t.TempDir()
	results := make(chan settings, 8)
	failures := make(chan error, 8)
	for range 8 {
		s := testSetup(t)
		s.Config.Dir = dir
		go func() {
			state, err := s.WriteOnce()
			if err != nil {
				failures <- err
				return
			}
			results <- state
		}()
	}
	var first settings
	for range 8 {
		select {
		case err := <-failures:
			t.Fatal(err)
		case state := <-results:
			if first.Password == "" {
				first = state
			}
			if first.Password != state.Password || first.OperationID != state.OperationID {
				t.Fatal("concurrent initialization returned different saved identities")
			}
		}
	}
	pkg, err := contract.LoadPackage(filepath.Join(dir, "examples", "hello", "pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, diagnostics := contract.Compile(pkg, nil); contract.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
}

func TestVerifiedHelperIsCopiedIntoPersistentSharedDirectory(t *testing.T) {
	s := testSetup(t)
	s.helper = []byte("verified helper content")
	source := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(source, s.helper, 0700); err != nil {
		t.Fatal(err)
	}
	s.Config.HelperPath = source
	if _, err := s.WriteOnce(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Config.HelperPath, s.Config.Dir+string(os.PathSeparator)) || s.Config.HelperPath == source {
		t.Fatal("helper still depends on archive location")
	}
	actual, err := os.ReadFile(s.Config.HelperPath)
	if err != nil || !bytes.Equal(actual, s.helper) {
		t.Fatal("saved helper differs from verified source")
	}
	info, err := os.Stat(s.Config.HelperPath)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 != 0111) {
		t.Fatal("saved helper is not executable")
	}
	if err = os.WriteFile(s.Config.HelperPath, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WriteOnce(); err == nil {
		t.Fatal("modified saved helper accepted")
	}
}
