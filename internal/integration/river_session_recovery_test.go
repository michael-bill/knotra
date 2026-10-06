//go:build unix

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	storedb "github.com/michael-bill/knotra/internal/store/db"
)

func TestRiverMCPInitializationIdentitySurvivesCrashBeforeDatabaseCommit(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY, KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for MCP initialization crash acceptance")
	}
	for _, mode := range []string{"rolled_back", "committed_after_process_loss"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			t.Setenv("KNOTRA_MCP_EVIDENCE_KEY", "fixture-key")
			var calls, initializations, deletes atomic.Int32
			var runSession atomic.Value
			server := mcp.NewServer(&mcp.Implementation{Name: "initialization-crash", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "stats"}, func(context.Context, *mcp.CallToolRequest, struct {
				Value int `json:"value"`
			}) (*mcp.CallToolResult, map[string]int, error) {
				calls.Add(1)
				return nil, map[string]int{"value": 42}, nil
			})
			transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Minute})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if id, ok := runSession.Load().(string); ok && req.Method == http.MethodDelete && req.Header.Get("Mcp-Session-Id") == id {
					if req.Header.Get("X-Cleanup-Key") != "fixture-key" || req.Header.Get("Mcp-Protocol-Version") == "" {
						t.Error("cleanup lost the admitted credential reference or protocol version")
					}
					if deletes.Add(1) == 1 {
						http.Error(w, "temporary cleanup failure", http.StatusServiceUnavailable)
						return
					}
				}
				if req.Method == http.MethodPost {
					body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
					if err != nil {
						t.Error(err)
						return
					}
					req.Body = io.NopCloser(bytes.NewReader(body))
					var message struct{ Method string }
					if err := json.Unmarshal(body, &message); err != nil {
						t.Error(err)
						return
					}
					if message.Method == "initialize" {
						response := httptest.NewRecorder()
						transport.ServeHTTP(response, req)
						if initializations.Add(1) == 2 {
							runSession.Store(response.Header().Get("Mcp-Session-Id"))
						}
						for key, values := range response.Header() {
							w.Header()[key] = values
						}
						w.WriteHeader(response.Code)
						_, _ = w.Write(response.Body.Bytes())
						return
					}
				}
				transport.ServeHTTP(w, req)
			}))
			defer remote.Close()
			profile := recoveryProfile()
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_MCP_EVIDENCE_KEY"}}
			profile.Spec.MCP = map[string]contract.MCPConnection{"data": {
				Transport: "streamable_http", URL: remote.URL, AllowRunSession: true,
				Headers:      map[string]contract.Credential{"X-Cleanup-Key": {SecretRef: "fixture"}},
				AllowedTools: []string{"stats"}, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}},
			}}
			work := t.TempDir()
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "mcp-evidence-crash"}
			application := "mcp-evidence-" + uuid.NewString()
			parsed, parseErr := url.Parse(options.DatabaseURL)
			if parseErr == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
				query := parsed.Query()
				query.Set("application_name", application)
				parsed.RawQuery = query.Encode()
				options.DatabaseURL = parsed.String()
			} else {
				options.DatabaseURL += " application_name=" + application
			}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			barrier, err := pgx.Connect(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Close(context.Background()) }()
			if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273649)"); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION mcp_identity_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273649); RETURN NEW; END $$;
		CREATE TRIGGER mcp_identity_crash_barrier BEFORE UPDATE ON knotra_resources FOR EACH ROW WHEN (OLD.mcp_session IS NULL AND NEW.mcp_session IS NOT NULL) EXECUTE FUNCTION mcp_identity_crash_barrier()`); err != nil {
				t.Fatal(err)
			}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			var definition struct{ Definition protocol.Definition }
			source := riverSessionRecoveryPipeline
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			var backend int
			for {
				if err := admin.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND query LIKE '%RecordOwnedMCPSession%' LIMIT 1),0)`, application).Scan(&backend); err != nil {
					t.Fatal(err)
				}
				if backend != 0 {
					break
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("MCP identity update did not reach its commit barrier", ctx.Err())
				}
			}
			var resource execution.ResourceRecord
			resource.EngineID, resource.Kind, resource.Lifetime, resource.State = admin.EngineID, "mcp_http", "run", "active"
			var missing bool
			var deadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT id,host_id,run_id,instance_id,attempt_number,worker_id,ownership_generation,mcp_session IS NULL,
		(SELECT execution_deadline FROM knotra_runs WHERE id=$1) FROM knotra_resources WHERE run_id=$1 AND kind='mcp_http'`, admitted.Run.ID).Scan(&resource.ID, &resource.HostID, &resource.Ownership.RunID, &resource.Ownership.InstanceID, &resource.Ownership.Number, &resource.Ownership.WorkerID, &resource.Ownership.Generation, &missing, &deadline); err != nil || !missing || calls.Load() != 0 {
				t.Fatalf("tool execution preceded initialization commit: missing identity=%v calls=%d error=%v", missing, calls.Load(), err)
			}
			files, err := execution.OpenOutcomeFiles(filepath.Join(options.DataDir, "outcomes"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = files.Close() }()
			identity, err := files.GetMCPSession(resource)
			if err != nil || identity == nil || identity.SessionID == "" || identity.SessionID != runSession.Load() || identity.Connection != "data" || initializations.Load() != 2 {
				t.Fatalf("initialization evidence was not durable before database commit: error=%v", err)
			}
			first.kill(t)
			killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || killed.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			if mode == "rolled_back" {
				// A single autocommit UPDATE may complete after client death. Terminate
				// only this fixture's blocked backend to exercise the aborted outcome too.
				var terminated bool
				if err := admin.Pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", backend).Scan(&terminated); err != nil || !terminated {
					t.Fatalf("did not terminate the fixture's blocked update: terminated=%v error=%v", terminated, err)
				}
			}
			if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273649)"); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER mcp_identity_crash_barrier ON knotra_resources"); err != nil {
				t.Fatal(err)
			}
			if err := admin.Pool.QueryRow(ctx, "SELECT mcp_session IS NULL FROM knotra_resources WHERE id=$1", resource.ID).Scan(&missing); err != nil || missing != (mode == "rolled_back") {
				t.Fatalf("identity update did not follow the injected outcome: mode=%s missing=%v error=%v", mode, missing, err)
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			instance := execution.StableID("n", admitted.Run.ID+"/root/first")
			for {
				var run struct{ Run protocol.Run }
				if err := api.Get(ctx, "/runs/"+admitted.Run.ID, &run); err != nil {
					t.Fatal(err)
				}
				waiting := false
				for _, node := range run.Run.Instances {
					waiting = waiting || (node.ID == instance && node.Status == "waiting_resolution")
				}
				if waiting {
					break
				}
				if protocol.Terminal(run.Run.Status) {
					t.Fatalf("replacement lost initialization uncertainty: status=%s", run.Run.Status)
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("replacement did not request resolution", ctx.Err())
				}
			}
			if err := api.Command(ctx, "/runs/"+admitted.Run.ID+"/instances/"+instance+"/resolve", map[string]any{"outcome": "failed", "evidence": "fixture confirms initialized session and zero tool calls"}, uuid.NewString(), new(any)); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			for {
				var closed, restored bool
				if err := admin.Pool.QueryRow(ctx, "SELECT state='closed',COALESCE(mcp_session->>'sessionId'=$2,false) FROM knotra_resources WHERE id=$1", resource.ID, identity.SessionID).Scan(&closed, &restored); err != nil {
					t.Fatal(err)
				}
				if closed && restored {
					break
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("replacement did not restore and delete the recorded session", ctx.Err())
				}
			}
			var attempts, debits, operations, published, slots int
			var recoveredDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
		(SELECT COALESCE(sum(used),0) FROM knotra_budgets WHERE run_id=$1),
			(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND (admitted_at IS NOT NULL OR completed)),
			(SELECT count(*) FROM knotra_artifacts WHERE published AND document->'origin'->>'runId'=$1),
			(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
			(SELECT execution_deadline FROM knotra_runs WHERE id=$1)`, final.ID).Scan(&attempts, &debits, &operations, &published, &slots, &recoveredDeadline); err != nil {
				t.Fatal(err)
			}
			probe, err := http.NewRequestWithContext(ctx, http.MethodGet, remote.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			probe.Header.Set("Mcp-Session-Id", identity.SessionID)
			probe.Header.Set("Mcp-Protocol-Version", identity.ProtocolVersion)
			probe.Header.Set("Accept", "text/event-stream")
			response, err := remote.Client().Do(probe)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if final.Status != "failed" || attempts != 1 || debits != 0 || operations != 0 || published != 0 || slots != 0 || !deadline.Equal(recoveredDeadline) || initializations.Load() != 2 || calls.Load() != 0 || deletes.Load() < 2 || response.StatusCode != http.StatusNotFound {
				t.Fatalf("status=%s attempts=%d debits=%d admitted operations=%d publications=%d slots=%d deadline=%v initializations=%d calls=%d deletes=%d remote=%d", final.Status, attempts, debits, operations, published, slots, deadline.Equal(recoveredDeadline), initializations.Load(), calls.Load(), deletes.Load(), response.StatusCode)
			}
			t.Logf("SIGKILL with %s identity update retained the immutable file and original owner; replacement retried DELETE without tool execution or a new session", mode)
		})
	}
}

// Uses the production service in separate processes. Neither its adapter cache
// nor an unfinished agent's workspace is a durable execution checkpoint.
func TestRiverProcessLossPreservesAgentAndMCPSessionUncertainty(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY, KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for agent/MCP process recovery")
	}
	t.Setenv("KNOTRA_SESSION_FIXTURE_KEY", "fixture-key")
	cases := []struct{ mode, outcome, boundary string }{
		{mode: "agent_prefix", outcome: "not_started"},
		{mode: "run_session", outcome: "failed"},
	}
	for _, outcome := range []string{"succeeded", "failed", "not_started"} {
		for _, boundary := range []string{"before_commit", "committed_before_receipt"} {
			cases = append(cases, struct{ mode, outcome, boundary string }{"agent_prefix", outcome, boundary})
		}
	}
	for _, test := range cases {
		name := test.mode
		if test.boundary != "" {
			name += "_" + test.outcome + "_" + test.boundary
		}
		t.Run(name, func(t *testing.T) {
			mode := test.mode
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			work := t.TempDir()
			effectsPath := filepath.Join(work, "external-effects.log")
			var turns, calls, initializations, deletes atomic.Int32
			var runSession atomic.Value
			type value struct {
				Value int `json:"value"`
			}
			mcpServer := mcp.NewServer(&mcp.Implementation{Name: "recovery-fixture", Version: "1"}, nil)
			mcp.AddTool(mcpServer, &mcp.Tool{Name: "stats", Description: "Measure a value"}, func(_ context.Context, req *mcp.CallToolRequest, input value) (*mcp.CallToolResult, value, error) {
				calls.Add(1)
				if mode == "agent_prefix" {
					// This parent-owned fixture records a real external write;
					// the killed service cannot roll it back or erase it.
					file, err := os.OpenFile(effectsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
					if err != nil {
						return nil, value{}, err
					}
					_, writeErr := fmt.Fprintf(file, "%d\n", input.Value)
					syncErr := file.Sync()
					closeErr := file.Close()
					if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
						return nil, value{}, err
					}
				}
				runSession.Store(req.Session.ID())
				return nil, input, nil
			})
			transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Minute})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					t.Error("replacement cleanup used the changed profile instead of the admitted connection")
					http.Error(w, "wrong endpoint", http.StatusNotFound)
					return
				}
				if id, ok := runSession.Load().(string); ok && r.Method == http.MethodDelete && r.Header.Get("Mcp-Session-Id") == id {
					if r.Header.Get("X-Recovery-Key") != "fixture-key" || r.Header.Get("Mcp-Protocol-Version") == "" {
						t.Error("replacement cleanup lost the frozen credential reference or protocol header")
					}
					if deletes.Add(1) == 1 {
						http.Error(w, "temporary cleanup failure", http.StatusServiceUnavailable)
						return
					}
				}
				if r.Method == http.MethodPost {
					data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
					if err != nil {
						t.Error(err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
					var message struct{ Method string }
					if err := json.Unmarshal(data, &message); err != nil {
						t.Error(err)
						return
					}
					if message.Method == "initialize" {
						initializations.Add(1)
					}
				}
				transport.ServeHTTP(w, r)
			}))
			defer remote.Close()
			blocked := make(chan struct{}, 1)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				if _, err := io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20)); err != nil {
					t.Error(err)
					return
				}
				turn := turns.Add(1)
				if turn >= 3 {
					select {
					case blocked <- struct{}{}:
					default:
					}
					select {
					case <-r.Context().Done():
					case <-ctx.Done():
					}
					return
				}
				name, args := "knotra_files_write", `{"path":"report.txt","content":"confirmed-prefix"}`
				if turn == 2 {
					name, args = "mcp_1", `{"value":42}`
				}
				encoded, _ := json.Marshal(args)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":%s}]}`, turn, turn, name, encoded)
			}))
			defer model.Close()
			profile := recoveryProfile()
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Limits.MaxModelCalls, profile.Spec.Limits.MaxToolCalls = 4, 4
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_SESSION_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: model.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			sandbox := profile.Spec.Sandboxes["python"]
			sandbox.AllowedTools = []string{"files.write"}
			profile.Spec.Sandboxes["python"] = sandbox
			profile.Spec.MCP = map[string]contract.MCPConnection{"data": {Transport: "streamable_http", URL: remote.URL, Headers: map[string]contract.Credential{"X-Recovery-Key": {SecretRef: "fixture"}}, AllowedTools: []string{"stats"}, AllowRunSession: true, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}}}}
			if mode == "agent_prefix" {
				connection := profile.Spec.MCP["data"]
				connection.ToolPolicies["stats"] = contract.ToolPolicy{Effect: "write"}
				profile.Spec.MCP["data"] = connection
			}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "session-recovery-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			source := riverSessionRecoveryPipeline
			target := "second"
			if mode == "agent_prefix" {
				source, target = riverAgentRecoveryPipeline, "write"
			}
			var definition struct{ Definition protocol.Definition }
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			var human protocol.HumanRequest
			if mode == "agent_prefix" {
				select {
				case <-blocked:
				case <-ctx.Done():
					t.Fatal("agent did not complete its prefix", ctx.Err())
				}
			} else {
				human = awaitHuman(t, ctx, api, admitted.Run.ID)
			}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var confirmed int
			var deadline time.Time
			var nodeDeadline *time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT execution_deadline,
                (SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id=$2),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed)
				FROM knotra_runs WHERE id=$1`, admitted.Run.ID, target).Scan(&deadline, &nodeDeadline, &confirmed); err != nil || confirmed != map[string]int{"agent_prefix": 2, "run_session": 1}[mode] {
				t.Fatalf("confirmed tool prefix=%d error=%v", confirmed, err)
			}
			initialized := initializations.Load()
			if initialized < 2 || calls.Load() != 1 {
				t.Fatal("run-scoped MCP session was not exercised before process loss")
			}
			var resourceID string
			var encodedIdentity []byte
			if err := admin.Pool.QueryRow(ctx, "SELECT id,mcp_session FROM knotra_resources WHERE run_id=$1 AND kind='mcp_http' AND state='active'", admitted.Run.ID).Scan(&resourceID, &encodedIdentity); err != nil {
				t.Fatal(err)
			}
			var identity execution.MCPSessionRecord
			if err := json.Unmarshal(encodedIdentity, &identity); err != nil || identity.SessionID != runSession.Load() || identity.Connection != "data" || strings.Contains(string(encodedIdentity), "fixture-key") {
				t.Fatalf("cleanup identity was not safely durable before process loss: error=%v", err)
			}
			var report []byte
			var sandboxID string
			if mode == "agent_prefix" {
				if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", admitted.Run.ID).Scan(&sandboxID); err != nil {
					t.Fatal(err)
				}
				report, err = exec.CommandContext(ctx, "docker", "exec", "sandbox-"+sandboxID, "cat", "/workspace/report.txt").CombinedOutput()
				if err != nil || string(report) != "confirmed-prefix" {
					t.Fatalf("agent file prefix=%s error=%v", report, err)
				}
				effects, err := os.ReadFile(effectsPath)
				if err != nil || string(effects) != "42\n" {
					t.Fatalf("agent external write prefix=%s error=%v", effects, err)
				}
			}
			first.kill(t)
			assertLifecycleSIGKILL(t, first)
			changedConnection := profile.Spec.MCP["data"]
			changedConnection.URL = remote.URL + "/replacement-config"
			profile.Spec.MCP["data"] = changedConnection
			writeJSON(t, profilePath, profile)
			first = startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if mode == "run_session" {
				recovered := awaitHuman(t, ctx, api, admitted.Run.ID)
				if recovered.ID != human.ID || !recovered.Deadline.Equal(human.Deadline) {
					t.Fatal("MCP wait identity changed on restart")
				}
				if _, err := executeCLI(ctx, api, "requests", "respond", human.ID, "--output", "approved=true"); err != nil {
					t.Fatal(err)
				}
			}
			var instance string
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for instance == "" {
				var response struct{ Run protocol.Run }
				if err := api.Get(ctx, "/runs/"+admitted.Run.ID, &response); err != nil {
					t.Fatal(err)
				}
				if protocol.Terminal(response.Run.Status) {
					t.Fatalf("lost execution did not request resolution: %+v", response.Run)
				}
				for _, node := range response.Run.Instances {
					if node.NodeID == target && node.Status == "waiting_resolution" {
						instance = node.ID
					}
				}
				if instance == "" {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal("replacement did not reconcile the lost execution", ctx.Err())
					}
				}
			}
			request, err := admin.Request(ctx, execution.StableID("q", instance+"/resolution/1"))
			if err != nil || request.Failure == nil || !request.Failure.Unknown || (mode == "agent_prefix" && request.Failure.CanRetryIfNotExecuted) {
				t.Fatalf("lost attempt resolution=%+v error=%v", request, err)
			}
			if nodeDeadline == nil {
				if mode == "agent_prefix" {
					t.Fatal("claimed agent did not retain its node deadline")
				}
				nodeDeadline = &request.Deadline
			}

			path := "/runs/" + admitted.Run.ID + "/instances/" + instance + "/resolve"
			payload := map[string]any{"outcome": test.outcome, "evidence": "fixture confirms the completed file and external write prefix; operator inspected the lost process"}
			var restoredArtifact contract.Artifact
			if test.outcome == "succeeded" {
				// Only inspected bytes are supplied by the operator, never a
				// replacement execution of the unfinished agent.
				var uploaded struct{ Artifact contract.Artifact }
				if err := api.Command(ctx, "/artifacts", map[string]any{"name": "report.txt", "mediaType": "text/plain", "content": base64.StdEncoding.EncodeToString(report)}, uuid.NewString(), &uploaded); err != nil {
					t.Fatal(err)
				}
				restoredArtifact = uploaded.Artifact
				payload["outputs"] = map[string]any{"answer": 42, "report": restoredArtifact.ID}
			}
			key := uuid.NewString()
			if test.boundary == "" {
				if err := api.Command(ctx, path, payload, key, new(any)); err != nil {
					t.Fatal(err)
				}
			} else {
				schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
				if err != nil {
					t.Fatal(err)
				}
				deliveries, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
				if err != nil {
					t.Fatal(err)
				}
				queueNames := []string{"knotra_advance_" + admin.EngineID, "knotra_execute_" + admin.EngineID}
				for _, name := range queueNames {
					if err := deliveries.QueuePause(ctx, name, nil); err != nil {
						t.Fatal(err)
					}
				}
				commands, received, release := commandReceiptProxy(t, ctx, api, "/v1"+path, filepath.Join(work, "decision-client"), test.boundary == "committed_before_receipt")
				var barrier *pgx.Conn
				if test.boundary == "before_commit" {
					barrier, err = pgx.ConnectConfig(ctx, admin.Pool.Config().ConnConfig.Copy())
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = barrier.Close(context.Background()) }()
					if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273659)"); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION agent_decision_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273659); RETURN NEW; END $$;
                        CREATE TRIGGER agent_decision_crash_barrier BEFORE UPDATE ON knotra_requests FOR EACH ROW WHEN (OLD.status='open' AND NEW.status='resolved') EXECUTE FUNCTION agent_decision_crash_barrier()`); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() { done <- commands.Command(ctx, path, payload, key, new(any)) }()
				var receipt []byte
				if barrier != nil {
					awaitLifecycleBoundary(t, ctx, first, filepath.Join(work, "after.log"), func() bool {
						var waiting bool
						if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrier.PgConn().PID())).Scan(&waiting); err != nil {
							t.Fatal(err)
						}
						select {
						case err := <-done:
							t.Fatalf("decision returned before its barrier: %v", err)
						default:
						}
						return waiting
					})
				} else {
					select {
					case held := <-received:
						if held.status != http.StatusAccepted {
							t.Fatalf("held agent decision receipt=%d %s", held.status, held.data)
						}
						receipt = held.data
					case err := <-done:
						t.Fatalf("decision returned before its receipt was withheld: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				first.kill(t)
				assertLifecycleSIGKILL(t, first)
				release()
				if barrier != nil {
					if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273659)"); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER agent_decision_crash_barrier ON knotra_requests"); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("client confirmed the decision before recovery")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				pending, err := commands.Commands()
				if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
					t.Fatalf("client lost pending agent decision: %+v %v", pending, err)
				}
				var status, nodeState, stopCode string
				var responseID *string
				var accepted *time.Time
				var receipts int
				var paused bool
				if err := admin.Pool.QueryRow(ctx, `SELECT q.status,q.response_id,q.accepted_at,n.state,r.admission_paused,
                    (SELECT count(*) FROM knotra_commands WHERE id=$2),COALESCE(r.stop_cause->>'code','')
                    FROM knotra_requests q JOIN knotra_execution_nodes n ON n.run_id=q.run_id AND n.id=q.document->>'instanceId'
                    JOIN knotra_runs r ON r.id=q.run_id WHERE q.id=$1`, request.ID, key).Scan(&status, &responseID, &accepted, &nodeState, &paused, &receipts, &stopCode); err != nil {
					t.Fatal(err)
				}
				if test.boundary == "before_commit" {
					if status != "open" || nodeState != "waiting_resolution" || responseID != nil || accepted != nil || !paused || receipts != 0 || stopCode != "" {
						t.Fatalf("uncommitted agent decision survived: %s/%s response=%v accepted=%v paused=%t receipts=%d stop=%s", status, nodeState, responseID, accepted, paused, receipts, stopCode)
					}
				} else {
					wantNode, wantStop := "failed", "EXTERNAL_FAILED"
					if test.outcome == "not_started" {
						wantStop = "NOT_EXECUTED"
					}
					if test.outcome == "succeeded" {
						wantNode, wantStop = "succeeded", ""
					}
					if status != "resolved" || nodeState != wantNode || responseID == nil || *responseID != key || accepted == nil || !accepted.Before(request.Deadline) || receipts != 1 || stopCode != wantStop {
						t.Fatalf("committed agent decision changed: %s/%s response=%v accepted=%v receipts=%d stop=%s", status, nodeState, responseID, accepted, receipts, stopCode)
					}
				}
				startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "decision-recovered.log"), api)
				var replay json.RawMessage
				if err := commands.Retry(ctx, key, &replay); err != nil {
					t.Fatal(err)
				}
				if receipt != nil && !bytes.Equal(receipt, replay) {
					t.Fatal("agent decision retry replaced its committed receipt")
				}
				var duplicate json.RawMessage
				if err := api.Command(ctx, path, payload, key, &duplicate); err != nil || !bytes.Equal(duplicate, replay) {
					t.Fatalf("fresh client changed agent decision receipt: %v", err)
				}
				if err := api.Command(ctx, path, payload, uuid.NewString(), new(any)); err == nil {
					t.Fatal("second key replaced the accepted agent decision")
				} else {
					var conflict *client.HTTPError
					if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
						t.Fatalf("second agent decision error=%v", err)
					}
				}
				for _, name := range queueNames {
					if err := deliveries.QueueResume(ctx, name, nil); err != nil {
						t.Fatal(err)
					}
				}
			}

			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			var attempts, toolBudget, models, slots, published int
			var recoveredDeadline, recoveredNodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_artifacts WHERE published AND document->'origin'->>'runId'=$1),
				(SELECT execution_deadline FROM knotra_runs WHERE id=$1),
                (SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id=$2),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed)`, final.ID, target).Scan(&attempts, &toolBudget, &models, &slots, &published, &recoveredDeadline, &recoveredNodeDeadline, &confirmed); err != nil {
				t.Fatal(err)
			}
			wantAttempts, wantTools, wantModels := 2, 1, 0
			if mode == "agent_prefix" {
				wantAttempts, wantTools, wantModels = 1, 2, 3
			}
			wantStatus := "failed"
			if test.outcome == "succeeded" {
				wantStatus = "succeeded"
			}
			if final.Status != wantStatus || calls.Load() != 1 || initializations.Load() != initialized || turns.Load() != int32(wantModels) || attempts != wantAttempts || toolBudget != wantTools || models != wantModels || slots != 0 || published != 0 || !deadline.Equal(recoveredDeadline) || !nodeDeadline.Equal(recoveredNodeDeadline) || confirmed != wantTools {
				t.Fatalf("status=%s MCP calls=%d initializations=%d model calls=%d attempts=%d budgets=%d/%d slots=%d publications=%d deadline preserved=%v confirmed tools=%d", final.Status, calls.Load(), initializations.Load(), turns.Load(), attempts, toolBudget, models, slots, published, deadline.Equal(recoveredDeadline), confirmed)
			}
			var responseID *string
			var decision, evidence, operationID string
			var receipts, operations, completed, workers int
			var requestDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT q.response_id,q.response->>'decision',q.response->>'evidence',q.response->>'operationId',
                (q.document->>'deadline')::timestamptz,(SELECT count(*) FROM knotra_commands WHERE id=$2),
                (SELECT count(*) FROM knotra_operations WHERE run_id=$3 AND kind IN ('model','tool')),(SELECT count(*) FROM knotra_operations WHERE run_id=$3 AND kind IN ('model','tool') AND completed),
                (SELECT count(*) FROM knotra_workers) FROM knotra_requests q WHERE q.id=$1`, request.ID, key, final.ID).Scan(&responseID, &decision, &evidence, &operationID, &requestDeadline, &receipts, &operations, &completed, &workers); err != nil {
				t.Fatal(err)
			}
			wantDecision := map[string]string{"succeeded": "completed", "failed": "failed", "not_started": "not_executed"}[test.outcome]
			wantWorkers, wantOperations, wantCompleted := 2, 2, 1
			if test.boundary != "" {
				wantWorkers = 3
			}
			if mode == "agent_prefix" {
				wantOperations, wantCompleted = 5, 4
			}
			if responseID == nil || *responseID != key || decision != wantDecision || evidence != payload["evidence"] || operationID != request.Failure.OperationID || !requestDeadline.Equal(request.Deadline) || receipts != 1 || operations != wantOperations || completed != wantCompleted || workers != wantWorkers {
				t.Fatalf("agent/session decision=%s/%s evidence=%s operations=%d/%d receipts=%d workers=%d deadline=%v", decision, operationID, evidence, completed, operations, receipts, workers, requestDeadline)
			}
			if mode == "agent_prefix" {
				effects, err := os.ReadFile(effectsPath)
				if err != nil || string(effects) != "42\n" {
					t.Fatalf("operator decision repeated the external write: %s %v", effects, err)
				}
			}
			if test.outcome == "succeeded" {
				data, err := api.Bytes(ctx, "/artifacts/"+restoredArtifact.ID+"/content")
				if err != nil || !bytes.Equal(data, report) || string(final.Outputs["answer"]) != "42" {
					t.Fatalf("operator-confirmed completion changed inspected outputs: %s %v", data, err)
				}
				var reportID string
				if err := admin.Pool.QueryRow(ctx, `SELECT outputs->'report'->'artifacts'->0->>'id' FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2`, final.ID, instance).Scan(&reportID); err != nil || reportID != restoredArtifact.ID {
					t.Fatalf("confirmed agent report changed: %s %v", reportID, err)
				}
			} else if len(final.Outputs) != 0 || len(final.Artifacts) != 0 {
				t.Fatal("failed agent/session published root exports")
			}

			cleanup, stopCleanup := context.WithTimeout(ctx, 25*time.Second)
			defer stopCleanup()
			for {
				var state string
				if err := admin.Pool.QueryRow(cleanup, "SELECT state FROM knotra_resources WHERE id=$1", resourceID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state == "closed" {
					var open int
					if err := admin.Pool.QueryRow(cleanup, "SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed'", final.ID).Scan(&open); err != nil {
						t.Fatal(err)
					}
					if open == 0 {
						break
					}
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-cleanup.Done():
					t.Fatal("replacement did not delete the abandoned remote session", cleanup.Err())
				}
			}
			if sandboxID != "" {
				output, err := exec.CommandContext(cleanup, "docker", "inspect", "sandbox-"+sandboxID).CombinedOutput()
				if err == nil || !bytes.Contains(bytes.ToLower(output), []byte("no such object")) {
					t.Fatalf("resolved agent retained its sandbox: %s %v", output, err)
				}
			}
			probe, err := http.NewRequestWithContext(cleanup, http.MethodGet, remote.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			probe.Header.Set("Mcp-Session-Id", identity.SessionID)
			probe.Header.Set("Mcp-Protocol-Version", identity.ProtocolVersion)
			probe.Header.Set("Accept", "text/event-stream")
			response, err := remote.Client().Do(probe)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNotFound || deletes.Load() < 2 || calls.Load() != 1 || initializations.Load() != initialized {
				t.Fatalf("remote cleanup was not confirmed or replayed work: status=%d deletes=%d calls=%d initializations=%d", response.StatusCode, deletes.Load(), calls.Load(), initializations.Load())
			}
			t.Log("SIGKILL retained confirmed tool evidence and resolved uncertainty without replay; replacement retried DELETE of the durably recorded session and confirmed its removal")
		})
	}
}

const riverAgentRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: agent-prefix-recovery}
spec:
  models: {writer: {connection: cloud, requires: [toolCalling, structuredOutput]}}
  mcp: {data: {connection: data, session: run}}
  sandboxes: {box: {profile: python}}
  defaults:
    tools: {mcp: {data: [stats]}}
  nodes:
    write:
      type: agent
      sandbox: box
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      tools: {sandbox: [files.write]}
      agent: {model: writer, maxSteps: 4, prompt: {text: Write then measure}}
      outputs:
        answer: {schema: {type: integer}}
        report: {artifact: {mediaTypes: [text/plain]}, collect: {path: report.txt, mediaType: text/plain}}
  outputs:
    answer: {schema: {type: integer}, bind: {from: nodes.write.outputs.answer}}
`

const riverSessionRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: mcp-session-recovery}
spec:
  mcp: {data: {connection: data, session: run}}
  nodes:
    first:
      type: tool
      tool: {server: data, name: stats, arguments: {expr: '{"value":42}'}}
      outputs: {result: {schema: {type: object}}}
    review:
      type: human
      needs: [first]
      human: {prompt: {text: Continue}}
      outputs: {approved: {schema: {type: boolean}}}
    second:
      type: tool
      needs: [review]
      tool: {server: data, name: stats, arguments: {expr: '{"value":42}'}}
      outputs: {result: {schema: {type: object}}}
  outputs:
    result: {schema: {type: object}, bind: {from: nodes.second.outputs.result}}
`
