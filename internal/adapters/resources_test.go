package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func TestMCPCleanupRejectsUnknownAndForeignIdentity(t *testing.T) {
	runner := &Runner{EngineID: "engine", HostID: "host"}
	resource := execution.ResourceRecord{ID: uuid.NewString(), EngineID: "engine", HostID: "host", Kind: "mcp_http", State: "cleaning",
		Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "node", Number: 1}, WorkerID: "worker", Generation: 1}}
	if err := runner.CleanupMCPSession(t.Context(), resource, contract.Profile{}); err == nil {
		t.Fatal("unanswered initialization was treated as a deleted session")
	}
	resource.MCP = &execution.MCPSessionRecord{Connection: "tools", ProtocolVersion: "2025-11-25"}
	if err := runner.CleanupMCPSession(t.Context(), resource, contract.Profile{}); err != nil {
		t.Fatalf("stateless connection required a remote deletion: %v", err)
	}
	foreign := resource
	foreign.HostID = "another-host"
	if err := runner.CleanupMCPSession(t.Context(), foreign, contract.Profile{}); err == nil {
		t.Fatal("foreign host authorized MCP cleanup")
	}
	resource.MCP.SessionID = "original-session"
	if err := runner.CleanupMCPSession(t.Context(), resource, contract.Profile{}); err == nil {
		t.Fatal("cleanup selected a connection without its admitted profile")
	}
}

func TestMCPCleanupCancelsRecordedCallBeforeDeleting(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			files, err := execution.OpenOutcomeFiles(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = files.Close() }()
			runner := &Runner{EngineID: "engine", HostID: "host", ResourceEvidence: files}
			resource := execution.ResourceRecord{ID: uuid.NewString(), EngineID: "engine", HostID: "host", Kind: "mcp_http", Lifetime: "run", State: "cleaning",
				Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "node", Number: 1}, WorkerID: "worker", Generation: 1},
				MCP:       &execution.MCPSessionRecord{Connection: "tools", SessionID: "original", ProtocolVersion: "2025-11-25"}}
			if err := files.PutMCPSession(resource); err != nil {
				t.Fatal(err)
			}
			call := &mcpCallEvidence{files: files, resource: resource, admit: func() error {
				id, err := files.GetMCPCall(resource)
				if err != nil || string(id) != "42" {
					t.Fatalf("physical admission preceded durable RPC identity: id=%s error=%v", id, err)
				}
				return nil
			}}
			// The ID precedes potentially large arguments in the actual SDK envelope.
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://fixture.invalid", strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"arguments":"`+strings.Repeat("x", 1<<20)+`"}}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := call.beforeSend(request); err != nil {
				t.Fatal(err)
			}
			var methods []string
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Mcp-Session-Id") != "original" || request.Header.Get("Mcp-Protocol-Version") != "2025-11-25" || request.Header.Get("X-Key") != "pinned" {
					t.Error("cleanup lost its original session, protocol or credential")
				}
				methods = append(methods, request.Method)
				if request.Method == http.MethodPost {
					var notification struct {
						Method string `json:"method"`
						Params struct {
							RequestID int `json:"requestId"`
						} `json:"params"`
					}
					if err := json.NewDecoder(request.Body).Decode(&notification); err != nil || notification.Method != "notifications/cancelled" || notification.Params.RequestID != 42 {
						t.Errorf("wrong cancellation identity: %+v, %v", notification, err)
					}
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer remote.Close()
			credential := "pinned"
			profile := contract.Profile{Spec: contract.ProfileSpec{MCP: map[string]contract.MCPConnection{"tools": {Transport: "streamable_http", URL: remote.URL, Headers: map[string]contract.Credential{"X-Key": {Value: &credential}}}}}}
			err = runner.CleanupMCPSession(t.Context(), resource, profile)
			want := []string{http.MethodPost}
			if status == http.StatusAccepted {
				want = append(want, http.MethodDelete)
			}
			if (err != nil) != (status == http.StatusServiceUnavailable) || !reflect.DeepEqual(methods, want) {
				t.Fatalf("cleanup error=%v methods=%v want=%v", err, methods, want)
			}
			methods = nil
			if err := files.PutMCPCall(resource, call.id, true); err != nil {
				t.Fatal(err)
			}
			if err := runner.CleanupMCPSession(t.Context(), resource, profile); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(methods, []string{http.MethodDelete}) {
				t.Fatalf("confirmed call was cancelled: %v", methods)
			}
		})
	}
}

type resourceCloseHooks struct {
	*memoryHooks
	closed bool
}

func (h *resourceCloseHooks) CloseResource(context.Context, string) error {
	h.closed = true
	return nil
}

func TestSandboxClosePreservesFailedAndUnansweredCreationEvidence(t *testing.T) {
	for _, mode := range []string{"delete_failed", "deleted", "creation_unanswered"} {
		t.Run(mode, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "sandbox")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "lease"), []byte("watchdog"), 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "DELETE" {
					t.Errorf("unexpected method=%s", r.Method)
				}
				if mode == "delete_failed" {
					w.WriteHeader(500)
				} else {
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			hooks := &resourceCloseHooks{memoryHooks: newHooks()}
			resource := execution.ResourceRecord{ID: uuid.NewString(), Kind: "sandbox", Lifetime: "attempt"}
			sandbox := &sandbox{docker: &dockerClient{client: server.Client(), base: server.URL}, id: "known-container", dir: directory, dirCreated: true, creationAttempted: true, resource: resource, hooks: hooks}
			if mode == "creation_unanswered" {
				sandbox.id = ""
			}
			err := sandbox.close()
			if (err != nil) != (mode == "delete_failed") || calls.Load() != 1 || hooks.closed != (mode == "deleted") {
				t.Fatalf("error=%v deletes=%d ledgerClosed=%v", err, calls.Load(), hooks.closed)
			}
			_, statErr := os.Stat(directory)
			if mode == "delete_failed" && statErr != nil {
				t.Fatal("failed deletion lost watchdog data")
			}
			if mode != "delete_failed" && !os.IsNotExist(statErr) {
				t.Fatalf("directory deletion=%v", statErr)
			}
		})
	}
}
