package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/protocol"
)

const minimalPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: example}
spec:
  nodes:
    choose:
      type: switch
      switch:
        cases: [{name: selected, when: "true"}]
        default: other
  outputs:
    route:
      schema: {type: string}
      bind: {from: nodes.choose.outputs.route}
`

func fixtureFile(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "pipeline.yaml")
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
func invoke(t *testing.T, args []string, input string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := Execute(context.Background(), args, Options{In: strings.NewReader(input), Out: &out, Err: &stderr, Getenv: func(string) string { return "" }})
	return out.String(), stderr.String(), err
}

func TestValidateOffline(t *testing.T) {
	file := fixtureFile(t, minimalPipeline)
	out, _, err := invoke(t, []string{"validate", file, "--json"}, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var response struct {
		Valid       bool                  `json:"valid"`
		Diagnostics []contract.Diagnostic `json:"diagnostics"`
	}
	if err = json.Unmarshal([]byte(out), &response); err != nil || !response.Valid {
		t.Fatalf("invalid response %s %v", out, err)
	}
	if len(response.Diagnostics) == 0 || response.Diagnostics[0].Code != "ADMISSION_PENDING" {
		t.Fatal("offline check claims admission")
	}
}
func TestInvalidPackageExitCode(t *testing.T) {
	file := fixtureFile(t, strings.Replace(minimalPipeline, "nodes.choose.outputs.route", "nodes.missing.outputs.route", 1))
	out, _, err := invoke(t, []string{"validate", file, "--json"}, "")
	if ExitCode(err) != 2 || !strings.Contains(out, `"valid":false`) {
		t.Fatalf("out=%s err=%v code=%d", out, err, ExitCode(err))
	}
}
func TestRunUsesStableDistinctMutationKeys(t *testing.T) {
	file := fixtureFile(t, minimalPipeline)
	var mu sync.Mutex
	keys := map[string]string{}
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/info" {
			fmt.Fprintf(w, `{"protocol":%q,"engineId":"engine","principalId":"user"}`, protocol.Version)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		mutations++
		keys[r.URL.Path] = r.Header.Get("Idempotency-Key")
		switch r.URL.Path {
		case "/v1/definitions":
			fmt.Fprint(w, `{"definition":{"id":"definition"}}`)
		case "/v1/runs":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if string(body["definitionId"]) != `"definition"` || string(body["inputs"]) != `{"count":3}` {
				t.Errorf("wrong request %s", body)
			}
			fmt.Fprint(w, `{"run":{"id":"run","status":"pending"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	args := []string{"--endpoint", server.URL, "--state-dir", t.TempDir(), "--json", "run", file, "--profile", "local", "--input", "count=3", "--idempotency-key", "stable"}
	for i := 0; i < 2; i++ {
		out, _, err := invoke(t, args, "")
		if err != nil || !strings.Contains(out, `"id":"run"`) {
			t.Fatalf("%s %v", out, err)
		}
	}
	if mutations != 2 || keys["/v1/runs"] != "stable" || keys["/v1/definitions"] == "stable" || keys["/v1/definitions"] == "" {
		t.Fatalf("invalid mutations %d %v", mutations, keys)
	}
}
func TestArtifactDownloadIntegrityBeforeWriting(t *testing.T) {
	data := []byte("verified\n")
	sum := sha256.Sum256(data)
	bad := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			if bad {
				w.Write([]byte("corrupt"))
			} else {
				w.Write(data)
			}
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"artifact": contract.Artifact{ID: "a", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}})
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "artifact.txt")
	args := []string{"--endpoint", server.URL, "artifacts", "download", "a", "--output", output}
	if _, _, err := invoke(t, args, ""); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatal("wrong stored artifact")
	}
	if _, _, err = invoke(t, args, ""); err == nil {
		t.Fatal("existing file overwritten without --force")
	}
	bad = true
	output = filepath.Join(t.TempDir(), "bad.txt")
	args[len(args)-1] = output
	if _, _, err = invoke(t, args, ""); err == nil {
		t.Fatal("corruption accepted")
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("corrupt file written")
	}
}
func TestHumanResponseTargetsExactRequest(t *testing.T) {
	var route string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			fmt.Fprintf(w, `{"protocol":%q,"engineId":"e","principalId":"p"}`, protocol.Version)
			return
		}
		route = r.URL.Path
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if string(body["outputs"]) != `{"approved":true}` {
			t.Errorf("unexpected outputs %s", body["outputs"])
		}
		fmt.Fprint(w, `{"accepted":true,"requestId":"request"}`)
	}))
	defer server.Close()
	_, _, err := invoke(t, []string{"--endpoint", server.URL, "--state-dir", t.TempDir(), "requests", "respond", "request", "--output", "approved=true", "--idempotency-key", "answer"}, "")
	if err != nil || route != "/v1/requests/request/response" {
		t.Fatalf("%s %v", route, err)
	}
}
func TestWatchJournalsBeforeTerminalDisplay(t *testing.T) {
	stateDir := t.TempDir()
	event := protocol.Event{ID: "event-1", RunID: "run", At: time.Now().UTC(), Type: "run", Message: "succeeded"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/info":
			fmt.Fprintf(w, `{"protocol":%q,"engineId":"e","principalId":"p"}`, protocol.Version)
		case "/v1/runs/run/events":
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "id: event-1\ndata: %s\n\n", data)
		case "/v1/runs/run":
			fmt.Fprint(w, `{"run":{"id":"run","status":"succeeded"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out, _, err := invoke(t, []string{"--endpoint", server.URL, "--state-dir", stateDir, "--json", "runs", "watch", "run"}, "")
	if err != nil || !strings.Contains(out, `"id":"event-1"`) {
		t.Fatalf("%s %v", out, err)
	}
	files, err := os.ReadDir(filepath.Join(stateDir, "events"))
	if err != nil || len(files) != 1 {
		t.Fatal("event journal missing")
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "events", files[0].Name()))
	if err != nil || !bytes.Contains(data, []byte(`"cursor":"event-1"`)) {
		t.Fatal("terminal cursor not committed")
	}
}
func TestInputRejectsDuplicateKeysAndAssignments(t *testing.T) {
	s := commandState{options: Options{In: strings.NewReader(`{"x":1,"x":2}`)}}
	if _, err := s.object("-", nil); err == nil {
		t.Fatal("duplicate JSON keys accepted")
	}
	if _, err := s.object("", []string{"x=1", "x=2"}); err == nil {
		t.Fatal("duplicate input assignments accepted")
	}
}
