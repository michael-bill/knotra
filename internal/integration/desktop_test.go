package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
)

// TestDesktopLiteralBackendCompatibility exercises existing browser and native
// clients against real services. Only the model provider is a deterministic HTTP
// fixture; scheduler, PostgreSQL, SSE, receipts, Python and file tools are real.
func TestDesktopLiteralBackendCompatibility(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DESKTOP") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_DESKTOP, KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for desktop backend acceptance")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".knotra/integration-work"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{execution.BackendTemporal, execution.BackendRiver} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
			defer cancel()
			work, err := os.MkdirTemp(filepath.Join(root, ".knotra/integration-work"), "desktop-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("desktop evidence retained at %s", work)
				} else if err := os.RemoveAll(work); err != nil {
					t.Error(err)
				}
			})
			var calls atomic.Int32
			provider := desktopLiteralProvider(t, &calls)
			defer provider.Close()
			profile := recoveryProfile()
			profile.Metadata.Name = "local"
			profile.Spec.Limits = contract.Limits{Timeout: "5m", MaxConcurrentNodes: 4, MaxNodeInstances: 100, MaxModelCalls: 100, MaxToolCalls: 100}
			box := profile.Spec.Sandboxes["python"]
			box.AllowedTools = []string{"files.read", "files.write", "process.exec"}
			profile.Spec.Sandboxes = map[string]contract.SandboxProfile{"python_box": box}
			profile.Spec.Models = map[string]contract.ModelConnection{"model_main": {Provider: "ollama", Model: "qwen3.5:9b", BaseURL: provider.URL}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: backend, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx),
				TemporalAddress: env("KNOTRA_TEST_TEMPORAL", "127.0.0.1:7233"), Namespace: "default", TaskQueue: "desktop-" + uuid.NewString(),
				CORSOrigin: "http://127.0.0.1:1420", DataDir: filepath.Join(work, "engine"), DockerHost: os.Getenv("DOCKER_HOST"),
				HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), FirewallImage: env("KNOTRA_TEST_FIREWALL_IMAGE", "knotra-firewall:dev"), Profiles: []string{profilePath}}
			if backend == execution.BackendRiver {
				options.TemporalAddress = "127.0.0.1:1"
			}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			engine := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "engine.log"), api)
			defer engine.kill(t)
			runClient := func(name string, args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, name, args...)
				cmd.Dir = filepath.Join(root, "app")
				cmd.Env = append(os.Environ(), "KNOTRA_E2E_ENDPOINT="+api.BaseURL)
				logPath := filepath.Join(work, name+".log")
				log, err := os.Create(logPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = log.Close() }()
				cmd.Stdout, cmd.Stderr = log, log
				if err := cmd.Run(); err != nil {
					t.Fatalf("%s acceptance failed: %v; see %s", name, err, logPath)
				}
			}
			runClient("cargo", "test", "--locked", "--manifest-path", "src-tauri/Cargo.toml", "real_engine", "--", "--ignored")
			runClient("bun", "run", "test:e2e", "tests/e2e/live-engine.spec.ts", "tests/e2e/starter-engine.spec.ts", "--workers=1",
				"--grep", "real Ollama run|real human review|real Qwen agent|hello starter streams", "--output", filepath.Join(work, "browser"))
			s, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var runs, succeeded, model, tool int
			if err := s.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE document->>'status'='succeeded'),
				COALESCE((SELECT sum(used) FROM knotra_budgets WHERE scope=run_id AND kind='model'),0),
				COALESCE((SELECT sum(used) FROM knotra_budgets WHERE scope=run_id AND kind='tool'),0) FROM knotra_runs`).Scan(&runs, &succeeded, &model, &tool); err != nil {
				t.Fatal(err)
			}
			if runs != 5 || succeeded != 5 || calls.Load() != 5 || model != 5 || tool != 5 {
				t.Fatalf("desktop changed work/receipts: runs=%d succeeded=%d physical calls=%d model=%d tool=%d", runs, succeeded, calls.Load(), model, tool)
			}
			t.Log("native SQLite receipts/SSE replay and four browser flows preserved five runs, five model calls and five tool debits")
		})
	}
}

func desktopLiteralProvider(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"qwen3.5:9b","digest":"desktop-literal-fixture"}]}`)
			return
		case "/api/show":
			_, _ = fmt.Fprint(w, `{"capabilities":["completion","tools"]}`)
			return
		case "/api/chat":
		default:
			http.NotFound(w, r)
			return
		}
		var request struct {
			Messages []struct{ Role string }
			Tools    []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid model request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		encoder := json.NewEncoder(w)
		if len(request.Tools) > 0 {
			turn := 0
			for _, message := range request.Messages {
				if message.Role == "tool" {
					turn++
				}
			}
			name, args := "knotra_finish", map[string]any{"summary": "Live agent observation works"}
			switch turn {
			case 0:
				name, args = "knotra_files_write", map[string]any{"path": "note.txt", "content": "Live agent observation works"}
			case 1:
				name, args = "knotra_files_read", map[string]any{"root": "workspace", "path": "note.txt"}
			}
			_ = encoder.Encode(map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": name, "arguments": args}}}}, "done": true, "done_reason": "stop", "prompt_eval_count": 12, "eval_count": 8})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = encoder.Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": `{"greeting":"Hello `}, "done": false})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_ = encoder.Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": `Knotra!"}`},
			"done": true, "done_reason": "stop", "prompt_eval_count": 12, "eval_count": 8})
	}))
}
