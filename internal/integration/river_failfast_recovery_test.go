//go:build unix

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
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

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRiverFailFastProcessCrashBoundaries(t *testing.T) {
	testRiverFailFastProcessCrashBoundaries(t, "mcp")
}

func TestRiverSandboxFailFastProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_HELPER for sandbox fail-fast SIGKILL checks")
	}
	testRiverFailFastProcessCrashBoundaries(t, "sandbox")
}

func TestRiverAgentFailFastProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_HELPER for agent fail-fast SIGKILL checks")
	}
	testRiverFailFastProcessCrashBoundaries(t, "agent")
}

func testRiverFailFastProcessCrashBoundaries(t *testing.T, peerKind string) {
	t.Helper()
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for fail-fast SIGKILL checks")
	}
	t.Setenv("KNOTRA_FAILFAST_FIXTURE_KEY", "fixture-key")
	for _, mode := range []string{"before_stop_commit", "committed_before_remote_stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			blocked, interrupted := make(chan struct{}, 1), make(chan struct{}, 1)
			failAllowed := make(chan struct{})
			var calls [3]atomic.Int32
			var models, agentModels, notifications, deletes, initializations atomic.Int32
			var allowCleanup atomic.Bool
			var sessionID atomic.Value
			type value struct {
				Value int `json:"value"`
			}
			server := mcp.NewServer(&mcp.Implementation{Name: "failfast-recovery", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "stats"}, func(call context.Context, request *mcp.CallToolRequest, input value) (*mcp.CallToolResult, value, error) {
				sessionID.Store(request.Session.ID())
				switch input.Value {
				case 41:
					calls[0].Add(1)
					return nil, input, nil
				case 42:
					calls[1].Add(1)
					select {
					case blocked <- struct{}{}:
					default:
					}
					select {
					case <-call.Done():
						select {
						case interrupted <- struct{}{}:
						default:
						}
						return nil, value{}, call.Err()
					case <-ctx.Done():
						return nil, value{}, ctx.Err()
					}
				default:
					calls[2].Add(1)
					t.Error("downstream tool ran after fail-fast")
					return nil, input, nil
				}
			})
			transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Minute})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				method := ""
				if request.Method == http.MethodPost {
					data, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
					if err != nil {
						t.Error(err)
						return
					}
					request.Body = io.NopCloser(bytes.NewReader(data))
					var message struct{ Method string }
					if err := json.Unmarshal(data, &message); err != nil {
						t.Error(err)
						return
					}
					method = message.Method
					if method == "initialize" {
						initializations.Add(1)
					}
				}
				id, ok := sessionID.Load().(string)
				cleanup := ok && request.Header.Get("Mcp-Session-Id") == id && (request.Method == http.MethodDelete || method == "notifications/cancelled")
				if cleanup {
					if request.Header.Get("X-Cleanup-Key") != "fixture-key" || request.Header.Get("Mcp-Protocol-Version") == "" {
						t.Error("fail-fast cleanup lost original session credentials/protocol")
					}
					// A transient remote cleanup outage keeps the active handler alive
					// through SIGKILL, so recovery must actually stop the physical call.
					if !allowCleanup.Load() {
						http.Error(w, "cleanup temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					if request.Method == http.MethodDelete {
						deletes.Add(1)
					} else {
						notifications.Add(1)
					}
				}
				transport.ServeHTTP(w, request)
			}))
			defer func() { cancel(); remote.Close() }()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				data, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil {
					t.Error(err)
					return
				}
				if peerKind == "agent" && bytes.Contains(data, []byte("fixture agent work")) {
					turn := agentModels.Add(1)
					if turn > 2 {
						t.Error("fail-fast agent made another model call")
					}
					name, arguments := "knotra_files_write", `{"path":"report.txt","content":"confirmed-agent-prefix"}`
					if turn == 2 {
						name, arguments = "mcp_1", `{"value":42}`
					}
					encoded, _ := json.Marshal(arguments)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":%q,"arguments":%s}]}`, turn, turn, name, encoded)
					return
				}
				models.Add(1)
				select {
				case <-failAllowed:
				case <-ctx.Done():
					return
				}
				http.Error(w, "fixture permanent model rejection", http.StatusBadRequest)
			}))
			defer func() { cancel(); provider.Close() }()
			work := t.TempDir()
			profile := recoveryProfile()
			if peerKind == "mcp" {
				profile.Spec.Sandboxes = nil
			}
			if peerKind == "agent" {
				profile.Spec.Limits.MaxModelCalls, profile.Spec.Limits.MaxToolCalls = 3, 3
				sandbox := profile.Spec.Sandboxes["python"]
				sandbox.AllowedTools = []string{"files.write"}
				profile.Spec.Sandboxes["python"] = sandbox
			}
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_FAILFAST_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profile.Spec.MCP = map[string]contract.MCPConnection{"data": {Transport: "streamable_http", URL: remote.URL, AllowRunSession: true, Headers: map[string]contract.Credential{"X-Cleanup-Key": {SecretRef: "fixture"}}, AllowedTools: []string{"stats"}, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}}}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "failfast-recovery-test"}
			if peerKind != "mcp" {
				options.DockerHost = failFastDockerHost(t, ctx, &allowCleanup)
			}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			engineID := engineIdentity(t, ctx, api)
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var barrier *pgx.Conn
			if mode == "before_stop_commit" {
				barrier, err = pgx.Connect(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273652)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION failfast_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273652); RETURN NEW; END $$;
					CREATE TRIGGER failfast_crash_barrier BEFORE UPDATE ON knotra_runs FOR EACH ROW WHEN (OLD.stop_cause IS NULL AND NEW.stop_cause IS NOT NULL) EXECUTE FUNCTION failfast_crash_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			var definition struct{ Definition protocol.Definition }
			source := riverFailFastRecoveryPipeline
			if peerKind != "mcp" {
				source = strings.Replace(source, "  mcp: {data: {connection: data, session: run}}", "  mcp: {data: {connection: data, session: run}}\n  sandboxes: {box: {profile: python}}", 1)
				peer := riverFailFastSandboxPeer
				if peerKind == "agent" {
					peer = riverFailFastAgentPeer
					source = strings.Replace(source, "requires: [structuredOutput]", "requires: [structuredOutput, toolCalling]", 1)
				}
				start, end := strings.Index(source, "    peer:\n"), strings.Index(source, "    fail:\n")
				source = source[:start] + peer + source[end:]
			}
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			var sandboxID string
			if peerKind != "sandbox" {
				select {
				case <-blocked:
				case <-ctx.Done():
					t.Fatal("peer MCP call did not start", ctx.Err())
				}
			} else {
				for {
					if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", admitted.Run.ID).Scan(&sandboxID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal(err)
					}
					if sandboxID != "" {
						data, err := exec.CommandContext(ctx, "docker", "exec", "sandbox-"+sandboxID, "cat", "/workspace/started").CombinedOutput()
						if err == nil && string(data) == "physical-execution\n" {
							break
						}
					}
					select {
					case <-time.After(50 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("sandbox process did not start", ctx.Err())
					}
				}
			}
			if peerKind == "agent" {
				if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", admitted.Run.ID).Scan(&sandboxID); err != nil {
					t.Fatal(err)
				}
			}
			var rootDeadline, peerDeadline time.Time
			var resourceID string
			var identity execution.MCPSessionRecord
			var encoded []byte
			if err := admin.Pool.QueryRow(ctx, `SELECT execution_deadline,(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='peer') FROM knotra_runs WHERE id=$1`, admitted.Run.ID).Scan(&rootDeadline, &peerDeadline); err != nil {
				t.Fatal(err)
			}
			if err := admin.Pool.QueryRow(ctx, "SELECT id,mcp_session FROM knotra_resources WHERE run_id=$1 AND kind='mcp_http'", admitted.Run.ID).Scan(&resourceID, &encoded); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &identity); err != nil || identity.SessionID != sessionID.Load() {
				t.Fatalf("run session identity=%+v error=%v", identity, err)
			}
			initialized := initializations.Load()
			close(failAllowed)
			for {
				var ready bool
				if mode == "before_stop_commit" {
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE '%SetRunStopCause%')`).Scan(&ready); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := admin.Pool.QueryRow(ctx, "SELECT stop_cause IS NOT NULL FROM knotra_runs WHERE id=$1", admitted.Run.ID).Scan(&ready); err != nil {
						t.Fatal(err)
					}
				}
				if ready {
					break
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("fail-fast publication did not reach crash boundary", ctx.Err())
				}
			}
			var outcomeKey string
			if err := admin.Pool.QueryRow(ctx, `SELECT a.outcome_key FROM knotra_execution_attempts a JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id WHERE a.run_id=$1 AND n.node_id='fail'`, admitted.Run.ID).Scan(&outcomeKey); err != nil {
				t.Fatal(err)
			}
			files, err := execution.OpenOutcomeFiles(filepath.Join(options.DataDir, "outcomes"))
			if err != nil {
				t.Fatal(err)
			}
			saved, readErr := files.Get(outcomeKey)
			closeErr := files.Close()
			if readErr != nil || closeErr != nil || saved.Failure == nil || saved.Failure.Code != "MODEL_REQUEST_REJECTED" {
				t.Fatalf("confirmed failure envelope=%+v read=%v close=%v", saved.Failure, readErr, closeErr)
			}
			first.kill(t)
			if first.command.ProcessState == nil {
				t.Fatal("engine did not exit after SIGKILL")
			}
			killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || killed.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			if peerKind == "sandbox" {
				data, err := exec.CommandContext(ctx, "docker", "exec", "sandbox-"+sandboxID, "python", "-c", "import pathlib; p=pathlib.Path('/workspace'); print((p/'started').read_text().strip()); pid=int((p/'pid').read_text()); print(pathlib.Path('/proc',str(pid),'cmdline').read_bytes().split(b'\\0')[0].decode())").CombinedOutput()
				if err != nil || string(data) != "physical-execution\npython\n" {
					t.Fatalf("sandbox business process did not survive engine SIGKILL: output=%s error=%v", data, err)
				}
			}
			if peerKind == "agent" {
				data, err := exec.CommandContext(ctx, "docker", "exec", "sandbox-"+sandboxID, "cat", "/workspace/report.txt").CombinedOutput()
				if err != nil || string(data) != "confirmed-agent-prefix" {
					t.Fatalf("confirmed agent file did not survive SIGKILL: output=%s error=%v", data, err)
				}
			}
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273652)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER failfast_crash_barrier ON knotra_runs"); err != nil {
					t.Fatal(err)
				}
			}
			var stopped, published bool
			var failState, attemptState string
			if err := admin.Pool.QueryRow(ctx, `SELECT r.stop_cause IS NOT NULL,n.state,a.state,a.evidence_key IS NOT NULL FROM knotra_runs r
				JOIN knotra_execution_nodes n ON n.run_id=r.id AND n.node_id='fail'
				JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id WHERE r.id=$1`, admitted.Run.ID).Scan(&stopped, &failState, &attemptState, &published); err != nil {
				t.Fatal(err)
			}
			if mode == "before_stop_commit" {
				if stopped || published || failState != "running" || attemptState != "claimed" {
					t.Fatalf("uncommitted fail-fast survived: stopped=%v published=%v node=%s attempt=%s", stopped, published, failState, attemptState)
				}
			} else if !stopped || !published || failState != "failed" || attemptState != "completed" {
				t.Fatalf("fail-fast publication was not atomic: stopped=%v published=%v node=%s attempt=%s", stopped, published, failState, attemptState)
			}
			select {
			case <-interrupted:
				t.Fatal("fixture peer stopped before recovery could demonstrate cancellation")
			default:
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != engineID {
				t.Fatal("engine identity changed on restart")
			}
			allowCleanup.Store(true)
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			if peerKind != "sandbox" {
				select {
				case <-interrupted:
				case <-time.After(40 * time.Second):
					t.Fatal("replacement failed to stop the original MCP call")
				}
			}
			for {
				var closed bool
				if err := admin.Pool.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM knotra_resources WHERE run_id=$1 AND state<>'closed')", admitted.Run.ID).Scan(&closed); err != nil {
					t.Fatal(err)
				}
				if closed {
					break
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("replacement failed to close run session", ctx.Err())
				}
			}
			if peerKind != "mcp" {
				data, err := exec.CommandContext(ctx, "docker", "inspect", "sandbox-"+sandboxID).CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(strings.ToLower(string(data)), "no such object: sandbox-"+sandboxID) {
					t.Fatalf("sandbox deletion is unproven: output=%s error=%v", data, err)
				}
				if err := filepath.WalkDir(filepath.Join(options.DataDir, "work"), func(path string, _ os.DirEntry, err error) error {
					if err == nil && strings.HasSuffix(path, "sandbox-"+sandboxID) {
						return errors.New("fail-fast retained sandbox workspace")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", admitted.Run.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("sandbox was repeated: resources=%d error=%v", count, err)
				}
			}
			var attempts, tools, modelBudget, confirmed, slots, requests, openRequests, retainedRequests, prefixEvents, failEvents, artifacts int
			var prefixState, prefixOutput, stopCode, peerState string
			var finalRootDeadline, finalPeerDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='cancelled'
					AND document->'failure'->>'unknown'='true' AND document->>'instanceId'=(SELECT id FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='peer')),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->'data'->>'nodeId'='prefix' AND document->'data'->>'status'='succeeded'),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->'data'->>'nodeId'='fail' AND document->'data'->>'status'='failed'),
				(SELECT count(*) FROM knotra_artifacts WHERE published AND document->'origin'->>'runId'=$1),
				(SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='prefix'),
				(SELECT outputs->'result'->>'json' FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='prefix'),
				COALESCE(stop_cause->>'code',''),execution_deadline,
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='peer'),
				(SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='peer') FROM knotra_runs WHERE id=$1`, final.ID).Scan(&attempts, &tools, &modelBudget, &confirmed, &slots, &requests, &openRequests, &retainedRequests, &prefixEvents, &failEvents, &artifacts, &prefixState, &prefixOutput, &stopCode, &finalRootDeadline, &finalPeerDeadline, &peerState); err != nil {
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
			var prefix value
			if err := json.Unmarshal([]byte(prefixOutput), &prefix); err != nil {
				t.Fatal(err)
			}
			// Before the stop commit, lease recovery can discover peer uncertainty
			// before applying the saved failure. Retain and close that evidence;
			// no open operator request may survive the definitive root failure.
			wantPeerCalls, wantNotifications := int32(1), int32(1)
			wantTools, wantModels, wantConfirmed, wantAgentModels := 2, 1, 1, int32(0)
			if peerKind == "sandbox" {
				wantPeerCalls, wantNotifications = 0, 0
			}
			if peerKind == "agent" {
				wantTools, wantModels, wantConfirmed, wantAgentModels = 3, 3, 2, 2
			}
			if final.Status != "failed" || len(final.Outputs) != 0 || len(final.Artifacts) != 0 || attempts != 3 || tools != wantTools || modelBudget != wantModels || confirmed != wantConfirmed || slots != 0 || requests > 1 || openRequests != 0 || retainedRequests != requests || prefixEvents != 1 || failEvents != 1 || artifacts != 0 || prefixState != "succeeded" || prefix.Value != 41 || stopCode != "MODEL_REQUEST_REJECTED" || peerState != "cancelled" || calls[0].Load() != 1 || calls[1].Load() != wantPeerCalls || calls[2].Load() != 0 || models.Load() != 1 || agentModels.Load() != wantAgentModels || notifications.Load() < wantNotifications || deletes.Load() < 1 || initializations.Load() != initialized || response.StatusCode != http.StatusNotFound || !rootDeadline.Equal(finalRootDeadline) || !peerDeadline.Equal(finalPeerDeadline) {
				t.Fatalf("status=%s attempts=%d budgets=%d/%d confirmed=%d slots=%d requests=%d/%d/%d events=%d/%d artifacts=%d prefix=%s:%s stop=%s peer=%s calls=%d/%d/%d models=%d cleanup=%d/%d init=%d/%d remote=%d deadlines=%v/%v", final.Status, attempts, tools, modelBudget, confirmed, slots, openRequests, retainedRequests, requests, prefixEvents, failEvents, artifacts, prefixState, prefixOutput, stopCode, peerState, calls[0].Load(), calls[1].Load(), calls[2].Load(), models.Load(), notifications.Load(), deletes.Load(), initializations.Load(), initialized, response.StatusCode, rootDeadline.Equal(finalRootDeadline), peerDeadline.Equal(finalPeerDeadline))
			}
			t.Logf("SIGKILL preserved confirmed sibling and failure envelope; replacement stopped original %s work, closed owned resources and retained fail-fast cause, one-time calls/events/debits and original deadlines", peerKind)
		})
	}
}

// Keep physical work alive through the crash, even if the original worker
// observes the stop first. The replacement must retry the same owned deletion.
func failFastDockerHost(t *testing.T, ctx context.Context, allowCleanup *atomic.Bool) string {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		data, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		host = strings.TrimSpace(string(data))
	}
	target, err := url.Parse(host)
	if err != nil || target.Scheme != "unix" || target.Path == "" {
		t.Fatalf("sandbox crash fixture requires a local Unix Docker socket: host=%q error=%v", host, err)
	}
	transport := &http.Transport{DialContext: func(call context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(call, "unix", target.Path)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = transport
	dir, err := os.MkdirTemp("/tmp", "knotra-failfast-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete && strings.Contains(request.URL.Path, "/containers/") && !allowCleanup.Load() {
			http.Error(w, "fixture Docker deletion temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, request)
	}))
	if err := server.Listener.Close(); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return "unix://" + socket
}

const riverFailFastSandboxPeer = `    peer:
      type: code
      sandbox: box
      needs: [prefix]
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      code:
        command:
          - python
          - -c
          - |
            import os, pathlib, time
            pathlib.Path('pid').write_text(str(os.getpid()))
            with open('started', 'a') as marker:
                marker.write('physical-execution\n')
            while True:
                time.sleep(1)
      outputs: {result: {schema: {type: object}}}
`

const riverFailFastAgentPeer = `    peer:
      type: agent
      sandbox: box
      needs: [prefix]
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      tools: {sandbox: [files.write], mcp: {data: [stats]}}
      agent: {model: writer, maxSteps: 3, prompt: {text: fixture agent work}}
      outputs:
        result: {schema: {type: object}}
        report: {artifact: {mediaTypes: [text/plain]}, collect: {path: report.txt, mediaType: text/plain}}
`

const riverFailFastRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: failfast-commit-recovery}
spec:
  limits: {timeout: 3m, maxConcurrentNodes: 2}
  models: {writer: {connection: cloud, requires: [structuredOutput]}}
  mcp: {data: {connection: data, session: run}}
  nodes:
    prefix:
      type: tool
      tool: {server: data, name: stats, arguments: {expr: '{"value":41}'}}
      outputs: {result: {schema: {type: object}}}
    peer:
      type: tool
      needs: [prefix]
      execution: {timeout: 2m, retry: {maxAttempts: 3, backoff: 1ms}}
      tool: {server: data, name: stats, arguments: {expr: '{"value":42}'}}
      outputs: {result: {schema: {type: object}}}
    fail:
      type: llm
      needs: [prefix]
      llm: {model: writer, prompt: {text: fixture permanent rejection}}
      outputs: {answer: {schema: {type: integer}}}
    after:
      type: tool
      needs: [peer, fail]
      tool: {server: data, name: stats, arguments: {expr: '{"value":99}'}}
      outputs: {result: {schema: {type: object}}}
  outputs:
    result: {schema: {type: object}, bind: {from: nodes.after.outputs.result}}
`
