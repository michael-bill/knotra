//go:build unix

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
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

type heldCommandReceipt struct {
	status int
	data   []byte
}

func commandReceiptProxy(t *testing.T, ctx context.Context, api *client.Client, path, stateDir string, holdResponse bool) (*client.Client, <-chan heldCommandReceipt, context.CancelFunc) {
	t.Helper()
	target, err := url.Parse(api.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "receipt unavailable", http.StatusBadGateway)
	}
	received := make(chan heldCommandReceipt, 1)
	hold, release := context.WithCancel(ctx)
	var dropped atomic.Bool
	proxy.ModifyResponse = func(response *http.Response) error {
		if !holdResponse || response.Request.Method != http.MethodPost || response.Request.URL.Path != path || !dropped.CompareAndSwap(false, true) {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil {
			return err
		}
		received <- heldCommandReceipt{status: response.StatusCode, data: data}
		<-hold.Done()
		return errors.New("fixture discarded the committed receipt")
	}
	transport := httptest.NewServer(proxy)
	t.Cleanup(func() { release(); transport.Close() })
	return &client.Client{BaseURL: transport.URL, StateDir: stateDir}, received, release
}

func TestRiverCancellationProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY, KNOTRA_TEST_DATABASE_URL and KNOTRA_TEST_HELPER for cancellation SIGKILL checks")
	}
	t.Setenv("KNOTRA_CANCEL_RECOVERY_KEY", "fixture-key")
	for _, mode := range []string{"before_commit", "committed_before_receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			var calls atomic.Int32
			blocked, interrupted := make(chan struct{}, 1), make(chan struct{}, 1)
			server := mcp.NewServer(&mcp.Implementation{Name: "cancel-recovery", Version: "1"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "stats"}, func(call context.Context, _ *mcp.CallToolRequest, input struct {
				Value int `json:"value"`
			}) (*mcp.CallToolResult, map[string]int, error) {
				calls.Add(1)
				if input.Value != 42 {
					t.Error("downstream tool ran after cancellation")
				}
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
					return nil, nil, call.Err()
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			})
			remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: time.Minute}))
			defer func() { cancel(); remote.Close() }()
			profile := recoveryProfile()
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_CANCEL_RECOVERY_KEY"}}
			profile.Spec.MCP = map[string]contract.MCPConnection{"data": {
				Transport: "streamable_http", URL: remote.URL, AllowRunSession: true,
				Headers:      map[string]contract.Credential{"X-Cleanup-Key": {SecretRef: "fixture"}},
				AllowedTools: []string{"stats"}, ToolPolicies: map[string]contract.ToolPolicy{"stats": {Effect: "read"}},
			}}
			work := t.TempDir()
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), HelperPath: os.Getenv("KNOTRA_TEST_HELPER"), Profiles: []string{profilePath}, Version: "cancel-recovery-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "admission-client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			engineID := engineIdentity(t, ctx, api)
			var definition struct{ Definition protocol.Definition }
			source := riverToolCancellationPipeline
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			select {
			case <-blocked:
			case <-ctx.Done():
				t.Fatal("physical MCP call did not start", ctx.Err())
			}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var rootDeadline, nodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT execution_deadline,
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='work')
				FROM knotra_runs WHERE id=$1`, admitted.Run.ID).Scan(&rootDeadline, &nodeDeadline); err != nil {
				t.Fatal(err)
			}
			var resourceID string
			var identity execution.MCPSessionRecord
			var encodedIdentity []byte
			if err := admin.Pool.QueryRow(ctx, "SELECT id,mcp_session FROM knotra_resources WHERE run_id=$1 AND kind='mcp_http'", admitted.Run.ID).Scan(&resourceID, &encodedIdentity); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encodedIdentity, &identity); err != nil {
				t.Fatal(err)
			}
			schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			queueName := "knotra_advance_" + admin.EngineID
			if err := queue.QueuePause(ctx, queueName, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			path := "/runs/" + admitted.Run.ID + "/cancel"
			commands, received, release := commandReceiptProxy(t, ctx, api, "/v1"+path, filepath.Join(work, "cancel-client"), mode == "committed_before_receipt")
			var barrier *pgx.Conn
			if mode == "before_commit" {
				barrier, err = pgx.Connect(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273650)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION cancel_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273650); RETURN NEW; END $$;
					CREATE TRIGGER cancel_crash_barrier BEFORE UPDATE ON knotra_runs FOR EACH ROW WHEN (NOT OLD.cancel_requested AND NEW.cancel_requested) EXECUTE FUNCTION cancel_crash_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			key := uuid.NewString()
			done := make(chan error, 1)
			go func() { done <- commands.Command(ctx, path, map[string]any{}, key, new(any)) }()
			var committedReceipt []byte
			if mode == "before_commit" {
				for {
					var waiting bool
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE '%SetRunCancelled%')`).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("cancel returned before its commit barrier: %v", err)
					case <-time.After(25 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			} else {
				select {
				case receipt := <-received:
					if receipt.status != http.StatusAccepted {
						t.Fatalf("held cancel receipt=%d %s", receipt.status, receipt.data)
					}
					committedReceipt = receipt.data
				case err := <-done:
					t.Fatalf("cancel returned before its receipt was held: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			first.kill(t)
			killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || killed.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			release()
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273650)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER cancel_crash_barrier ON knotra_runs"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("client received a cancel receipt before recovery")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			pending, err := commands.Commands()
			if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
				t.Fatalf("client lost its pending cancel command: commands=%+v error=%v", pending, err)
			}
			var cancelling bool
			var receipts int
			if err := admin.Pool.QueryRow(ctx, "SELECT cancel_requested,(SELECT count(*) FROM knotra_commands WHERE id=$2) FROM knotra_runs WHERE id=$1", admitted.Run.ID, key).Scan(&cancelling, &receipts); err != nil {
				t.Fatal(err)
			}
			if cancelling != (mode == "committed_before_receipt") || receipts != map[string]int{"before_commit": 0, "committed_before_receipt": 1}[mode] {
				t.Fatalf("cancel intent and receipt were not atomic: cancelling=%v receipts=%d", cancelling, receipts)
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != engineID {
				t.Fatal("engine identity changed on restart")
			}
			var replay json.RawMessage
			if err := commands.Retry(ctx, key, &replay); err != nil {
				t.Fatal(err)
			}
			if mode == "committed_before_receipt" && string(replay) != string(committedReceipt) {
				t.Fatal("cancel retry replaced the committed receipt")
			}
			var duplicate json.RawMessage
			if err := api.Command(ctx, path, map[string]any{}, key, &duplicate); err != nil || string(duplicate) != string(replay) {
				t.Fatalf("fresh client did not replay the cancel receipt: %v", err)
			}
			if err := queue.QueueResume(ctx, queueName, nil); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			select {
			case <-interrupted:
			case <-time.After(25 * time.Second):
				t.Fatal("cancel recovery did not stop the physical MCP call within 25 seconds")
			case <-ctx.Done():
				t.Fatal("cancel recovery did not stop the physical MCP call", ctx.Err())
			}
			for {
				var closed bool
				if err := admin.Pool.QueryRow(ctx, "SELECT state='closed' FROM knotra_resources WHERE id=$1", resourceID).Scan(&closed); err != nil {
					t.Fatal(err)
				}
				if closed {
					break
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("cancel recovery did not close its owned session", ctx.Err())
				}
			}
			var attempts, tools, slots, requests, publications int
			var finalRootDeadline, finalNodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1),
				(SELECT count(*) FROM knotra_artifacts WHERE published AND document->'origin'->>'runId'=$1),
				(SELECT count(*) FROM knotra_commands WHERE id=$2),
				(SELECT execution_deadline FROM knotra_runs WHERE id=$1),
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='work')`, final.ID, key).Scan(&attempts, &tools, &slots, &requests, &publications, &receipts, &finalRootDeadline, &finalNodeDeadline); err != nil {
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
			if final.Status != "cancelled" || len(final.Outputs) != 0 || len(final.Artifacts) != 0 || attempts != 1 || tools != 1 || calls.Load() != 1 || slots != 0 || requests != 0 || publications != 0 || receipts != 1 || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) || response.StatusCode != http.StatusNotFound {
				t.Fatalf("status=%s attempts=%d debits=%d calls=%d slots=%d requests=%d publications=%d receipts=%d deadlines=%v/%v remote=%d", final.Status, attempts, tools, calls.Load(), slots, requests, publications, receipts, rootDeadline.Equal(finalRootDeadline), nodeDeadline.Equal(finalNodeDeadline), response.StatusCode)
			}
			t.Log("SIGKILL preserved cancel/receipt atomicity; client replay kept one attempt/debit and original deadlines, stopped the physical call and closed its remote session")
		})
	}
}

func TestRiverHumanAnswerProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for human answer SIGKILL checks")
	}
	for _, mode := range []string{"before_commit", "committed_before_receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}, Version: "human-recovery-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "admission-client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			identity := engineIdentity(t, ctx, api)
			timeout := "1m"
			if mode == "committed_before_receipt" {
				timeout = "5s"
			}
			source := fmt.Sprintf(riverHumanRecoveryPipeline, timeout)
			var definition struct{ Definition protocol.Definition }
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			request := awaitHuman(t, ctx, api, admitted.Run.ID)
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			queueName := "knotra_advance_" + admin.EngineID
			if err := queue.QueuePause(ctx, queueName, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			parentID := execution.StableID("n", admitted.Run.ID+"/root/review")
			var parentDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, "SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", admitted.Run.ID, parentID).Scan(&parentDeadline); err != nil {
				t.Fatal(err)
			}
			commands, received, release := commandReceiptProxy(t, ctx, api, "/v1/requests/"+request.ID+"/response", filepath.Join(work, "response-client"), mode == "committed_before_receipt")
			key := uuid.NewString()
			answer := strings.Repeat("accepted-", 25000)
			payload := map[string]any{"outputs": map[string]any{"answer": answer}}
			var barrier *pgx.Conn
			if mode == "before_commit" {
				barrier, err = pgx.Connect(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273648)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION answer_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273648); RETURN NEW; END $$;
					CREATE TRIGGER answer_crash_barrier BEFORE UPDATE ON knotra_requests FOR EACH ROW WHEN (OLD.status='open' AND NEW.status='answered') EXECUTE FUNCTION answer_crash_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- commands.Command(ctx, "/requests/"+request.ID+"/response", payload, key, new(any)) }()
			var committedReceipt []byte
			if mode == "before_commit" {
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE '%AcceptHumanAnswer%')`).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("answer exited before commit barrier: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
					}
				}
			} else {
				select {
				case response := <-received:
					if response.status != http.StatusOK {
						t.Fatalf("held answer receipt=%d %s", response.status, response.data)
					}
					committedReceipt = response.data
				case err := <-done:
					t.Fatalf("answer exited before receipt was withheld: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			first.kill(t)
			if first.command.ProcessState == nil {
				t.Fatal("engine did not exit after SIGKILL")
			}
			killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || killed.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			release()
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273648)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER answer_crash_barrier ON knotra_requests"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("client received confirmation before process loss")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			pending, err := commands.Commands()
			if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
				t.Fatalf("client lost its pending command: %+v %v", pending, err)
			}
			var status string
			var responseID *string
			var accepted *time.Time
			var receipts, active, succeeded, collected int
			if err := admin.Pool.QueryRow(ctx, `SELECT q.status,q.response_id,q.accepted_at,
				(SELECT count(*) FROM knotra_commands WHERE id=$2),c.active_children,c.succeeded_children,c.collection_position
				FROM knotra_requests q JOIN knotra_execution_controls c ON c.run_id=q.run_id AND c.instance_id=$3 WHERE q.id=$1`, request.ID, key, parentID).Scan(&status, &responseID, &accepted, &receipts, &active, &succeeded, &collected); err != nil {
				t.Fatal(err)
			}
			if mode == "before_commit" {
				if status != "open" || responseID != nil || accepted != nil || receipts != 0 || active != 1 || succeeded != 0 || collected != 0 {
					t.Fatalf("uncommitted answer survived SIGKILL: status=%s response=%v accepted=%v receipts=%d control=%d/%d/%d", status, responseID, accepted, receipts, active, succeeded, collected)
				}
			} else if status != "accepted" || responseID == nil || *responseID != key || accepted == nil || !accepted.Before(request.Deadline) || receipts != 1 || active != 0 || succeeded != 1 || collected < 0 || collected > 1 {
				t.Fatalf("committed answer was not atomic: status=%s response=%v accepted=%v receipts=%d control=%d/%d/%d", status, responseID, accepted, receipts, active, succeeded, collected)
			}
			runStatus, err := store.ReadRunStatus(ctx, admin.Pool, admitted.Run.ID)
			if err != nil || protocol.Terminal(runStatus) {
				t.Fatalf("paused parent finished before recovery: %s %v", runStatus, err)
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != identity {
				t.Fatal("engine identity changed across restart")
			}
			if mode == "before_commit" {
				recovered := awaitHuman(t, ctx, api, admitted.Run.ID)
				if recovered.ID != request.ID || !recovered.Deadline.Equal(request.Deadline) {
					t.Fatal("open request identity/deadline changed on restart")
				}
			} else {
				select {
				case <-time.After(time.Until(request.Deadline) + 150*time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			var replay json.RawMessage
			if err := commands.Retry(ctx, key, &replay); err != nil {
				t.Fatal(err)
			}
			if mode == "committed_before_receipt" && string(replay) != string(committedReceipt) {
				t.Fatal("retry replaced the committed receipt")
			}
			var duplicate json.RawMessage
			if err := api.Command(ctx, "/requests/"+request.ID+"/response", payload, key, &duplicate); err != nil || string(duplicate) != string(replay) {
				t.Fatalf("fresh client did not replay the server receipt: %v", err)
			}
			if err := api.Command(ctx, "/requests/"+request.ID+"/response", payload, uuid.NewString(), new(any)); err == nil {
				t.Fatal("a second command replaced the first accepted answer")
			} else {
				var conflict *client.HTTPError
				if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
					t.Fatalf("second answer error=%v", err)
				}
			}
			if err := queue.QueueResume(ctx, queueName, nil); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			var answers []string
			if err := json.Unmarshal(final.Outputs["answers"], &answers); err != nil || final.Status != "succeeded" || len(answers) != 1 || answers[0] != answer {
				t.Fatalf("recovered status=%s export bytes=%d diagnostics=%+v error=%v", final.Status, len(final.Outputs["answers"]), final.Diagnostics, err)
			}
			var attempts, operations, debits, slots, successes int
			var finalParentDeadline, finalRequestDeadline time.Time
			var savedReceipt []byte
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1),
				COALESCE((SELECT sum(used) FROM knotra_budgets WHERE run_id=$1),0),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2),
				(q.document->>'deadline')::timestamptz,q.response_id,
				(SELECT response FROM knotra_commands WHERE id=$4)
				FROM knotra_requests q WHERE q.id=$3`, final.ID, parentID, request.ID, key).Scan(&attempts, &operations, &debits, &slots, &successes, &finalParentDeadline, &finalRequestDeadline, &responseID, &savedReceipt); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 || operations != 0 || debits != 0 || slots != 0 || successes != 2 || !parentDeadline.Equal(finalParentDeadline) || !request.Deadline.Equal(finalRequestDeadline) || responseID == nil || *responseID != key || string(savedReceipt) != string(replay) {
				t.Fatalf("attempts=%d operations=%d debits=%d slots=%d successes=%d parent deadline=%v request deadline=%v response=%v", attempts, operations, debits, slots, successes, parentDeadline.Equal(finalParentDeadline), request.Deadline.Equal(finalRequestDeadline), responseID)
			}
			t.Log("SIGKILL preserved answer/receipt/child completion atomically; client retry kept one answer, full 225 KB export and original deadlines")
		})
	}
}

func TestRiverResolutionProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for resolution SIGKILL checks")
	}
	t.Setenv("KNOTRA_RESOLUTION_FIXTURE_KEY", "fixture-key")
	for _, test := range []struct{ outcome, mode string }{
		{"succeeded", "before_commit"}, {"succeeded", "committed_before_receipt"},
		{"failed", "before_commit"}, {"failed", "committed_before_receipt"},
		{"not_started", "before_commit"}, {"not_started", "committed_before_receipt"},
	} {
		outcome, mode := test.outcome, test.mode
		t.Run(outcome+"/"+mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			var calls [3]atomic.Int32
			var generations [3]atomic.Int32
			var rejection atomic.Bool
			var initial atomic.Int32
			both := make(chan struct{})
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
				index := -1
				for i, marker := range []string{"fixture-a", "fixture-b", "fixture-c"} {
					if strings.Contains(string(data), marker) {
						index = i
						break
					}
				}
				if index < 0 {
					t.Errorf("provider call has no node marker")
					http.Error(w, "missing marker", 400)
					return
				}
				call := calls[index].Add(1)
				answer := 44
				if index < 2 {
					if initial.Add(1) == 2 {
						close(both)
					}
					select {
					case <-both:
					case <-ctx.Done():
						return
					}
					if outcome != "succeeded" && index == 1 && call == 1 {
						// A permanent provider rejection or verified non-execution is
						// externally recorded, but its response is lost with the process.
						if outcome == "failed" {
							rejection.Store(true)
						}
						select {
						case <-request.Context().Done():
						case <-ctx.Done():
						}
						if outcome == "failed" {
							http.Error(w, "fixture permanent model rejection", http.StatusBadRequest)
						}
						return
					}
					answer = 42
				}
				generations[index].Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"knotra_output","arguments":"{\"answer\":%d}"}]}`, answer)
			}))
			defer func() { cancel(); provider.Close() }()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profile.Spec.Limits.Timeout, profile.Spec.Limits.MaxModelCalls = "3m", 3
			if outcome == "not_started" {
				profile.Spec.Limits.MaxModelCalls = 4
			}
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_RESOLUTION_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}, Version: "resolution-recovery-test"}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "admission-client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			identity := engineIdentity(t, ctx, api)
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			// Real provider replies reach the engine, but their journal commits fail.
			// This produces two actual unknown outcomes without replaying the models.
			lostNodes := "('a','b')"
			if outcome != "succeeded" {
				lostNodes = "('a')"
			}
			if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION lose_resolution_fixture_reply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.completed AND EXISTS(SELECT 1 FROM knotra_execution_nodes WHERE run_id=NEW.run_id AND id=NEW.instance_id AND node_id IN `+lostNodes+`) THEN
					RAISE EXCEPTION 'fixture lost provider response journal'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER lose_resolution_fixture_reply BEFORE UPDATE ON knotra_operations FOR EACH ROW EXECUTE FUNCTION lose_resolution_fixture_reply()`); err != nil {
				t.Fatal(err)
			}
			var definition struct{ Definition protocol.Definition }
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: riverResolutionRecoveryPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(riverResolutionRecoveryPipeline)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var admitted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &admitted); err != nil {
				t.Fatal(err)
			}
			if outcome != "succeeded" {
				for {
					var open int
					if err := admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open'", admitted.Run.ID).Scan(&open); err != nil {
						t.Fatal(err)
					}
					if open == 1 && calls[1].Load() == 1 {
						break
					}
					select {
					case <-time.After(25 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("provider did not reach the unexecuted request", ctx.Err())
					}
				}
				first.kill(t)
				if first.command.ProcessState == nil {
					t.Fatal("provider-loss engine did not exit after SIGKILL")
				}
				killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || killed.Signal() != syscall.SIGKILL {
					t.Fatalf("provider-loss engine did not exit from SIGKILL: %v", first.command.ProcessState)
				}
				first = startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "lost-provider.log"), api)
				if generations[1].Load() != 0 {
					t.Fatal("provider generated an answer before the recorded rejection/non-execution")
				}
				if outcome == "failed" && !rejection.Load() {
					t.Fatal("provider did not record the permanent rejection before process loss")
				}
			}
			for {
				var open int
				if err := admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open'", admitted.Run.ID).Scan(&open); err != nil {
					t.Fatal(err)
				}
				if open == 2 {
					break
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("two provider outcomes did not become uncertain", ctx.Err())
				}
			}
			a := execution.StableID("n", admitted.Run.ID+"/root/a")
			b := execution.StableID("n", admitted.Run.ID+"/root/b")
			request, err := admin.Request(ctx, execution.StableID("q", b+"/resolution/1"))
			if err != nil || request.Failure == nil || !request.Failure.Unknown {
				t.Fatalf("resolution request=%+v error=%v", request, err)
			}
			if outcome == "not_started" && !request.Failure.CanRetryIfNotExecuted {
				t.Fatal("lost model request cannot be retried after verified non-execution")
			}
			var rootDeadline, nodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT execution_deadline,(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2) FROM knotra_runs WHERE id=$1`, admitted.Run.ID, b).Scan(&rootDeadline, &nodeDeadline); err != nil {
				t.Fatal(err)
			}
			schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := river.NewClient(riverpgxv5.New(admin.Pool), &river.Config{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			queueNames := []string{"knotra_advance_" + identity, "knotra_execute_" + identity}
			for _, name := range queueNames {
				if err := queue.QueuePause(ctx, name, nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			payload := map[string]any{"outcome": "succeeded", "evidence": "fixture provider recorded answer 42 before its journal commit failed", "outputs": map[string]any{"answer": 42}}
			if err := api.Command(ctx, "/runs/"+admitted.Run.ID+"/instances/"+a+"/resolve", payload, uuid.NewString(), new(any)); err != nil {
				t.Fatal(err)
			}
			var paused bool
			if err := admin.Pool.QueryRow(ctx, "SELECT admission_paused FROM knotra_runs WHERE id=$1", admitted.Run.ID).Scan(&paused); err != nil || !paused || calls[0].Load() != 1 || calls[1].Load() != 1 || calls[2].Load() != 0 {
				t.Fatalf("first resolution resumed root: paused=%v calls=%d/%d/%d error=%v", paused, calls[0].Load(), calls[1].Load(), calls[2].Load(), err)
			}
			payload["outcome"] = outcome
			if outcome != "succeeded" {
				delete(payload, "outputs")
				payload["evidence"] = "fixture provider recorded a permanent rejection before its response was lost with the engine"
			}
			if outcome == "not_started" {
				payload["evidence"] = "fixture confirms the received request performed zero model generations before process loss"
			}
			path := "/runs/" + admitted.Run.ID + "/instances/" + b + "/resolve"
			commands, received, release := commandReceiptProxy(t, ctx, api, "/v1"+path, filepath.Join(work, "resolution-client"), mode == "committed_before_receipt")
			key := uuid.NewString()
			var barrier *pgx.Conn
			if mode == "before_commit" {
				barrier, err = pgx.Connect(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273651)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION resolution_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273651); RETURN NEW; END $$;
					CREATE TRIGGER resolution_crash_barrier BEFORE UPDATE ON knotra_requests FOR EACH ROW WHEN (OLD.status='open' AND NEW.status='resolved') EXECUTE FUNCTION resolution_crash_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- commands.Command(ctx, path, payload, key, new(any)) }()
			var receipt []byte
			if mode == "before_commit" {
				for {
					var waiting bool
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE '%AcceptResolution%')`).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("resolution returned before commit barrier: %v", err)
					case <-time.After(25 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			} else {
				select {
				case held := <-received:
					if held.status != http.StatusAccepted {
						t.Fatalf("held resolution receipt=%d %s", held.status, held.data)
					}
					receipt = held.data
				case err := <-done:
					t.Fatalf("resolution returned before receipt was held: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			first.kill(t)
			if first.command.ProcessState == nil {
				t.Fatal("engine did not exit after SIGKILL")
			}
			killed, ok := first.command.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || killed.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			release()
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273651)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER resolution_crash_barrier ON knotra_requests"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("client confirmed resolution before recovery")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			pending, err := commands.Commands()
			if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
				t.Fatalf("client lost pending resolution: %+v %v", pending, err)
			}
			var status, nodeState, stopCode string
			var responseID *string
			var accepted *time.Time
			var receipts int
			if err := admin.Pool.QueryRow(ctx, `SELECT q.status,q.response_id,q.accepted_at,n.state,r.admission_paused,(SELECT count(*) FROM knotra_commands WHERE id=$2),COALESCE(r.stop_cause->>'code','')
				FROM knotra_requests q JOIN knotra_execution_nodes n ON n.run_id=q.run_id AND n.id=q.document->>'instanceId' JOIN knotra_runs r ON r.id=q.run_id WHERE q.id=$1`, request.ID, key).Scan(&status, &responseID, &accepted, &nodeState, &paused, &receipts, &stopCode); err != nil {
				t.Fatal(err)
			}
			if mode == "before_commit" {
				if status != "open" || nodeState != "waiting_resolution" || responseID != nil || accepted != nil || !paused || receipts != 0 || stopCode != "" {
					t.Fatalf("uncommitted resolution survived: status=%s node=%s response=%v accepted=%v paused=%v receipts=%d", status, nodeState, responseID, accepted, paused, receipts)
				}
			} else {
				wantNode, wantStop := "succeeded", ""
				if outcome == "failed" {
					wantNode, wantStop = "failed", "EXTERNAL_FAILED"
				}
				if outcome == "not_started" {
					wantNode = "retry_wait"
				}
				if status != "resolved" || nodeState != wantNode || responseID == nil || *responseID != key || accepted == nil || !accepted.Before(request.Deadline) || (outcome != "failed" && paused) || receipts != 1 || stopCode != wantStop {
					t.Fatalf("committed resolution was not atomic: status=%s node=%s response=%v accepted=%v paused=%v receipts=%d stop=%s", status, nodeState, responseID, accepted, paused, receipts, stopCode)
				}
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != identity {
				t.Fatal("engine identity changed on restart")
			}
			var replay json.RawMessage
			if err := commands.Retry(ctx, key, &replay); err != nil {
				t.Fatal(err)
			}
			if mode == "committed_before_receipt" && string(replay) != string(receipt) {
				t.Fatal("resolution retry replaced committed receipt")
			}
			var duplicate json.RawMessage
			if err := api.Command(ctx, path, payload, key, &duplicate); err != nil || string(duplicate) != string(replay) {
				t.Fatalf("fresh client did not replay resolution receipt: %v", err)
			}
			if err := api.Command(ctx, path, payload, uuid.NewString(), new(any)); err == nil {
				t.Fatal("another key replaced accepted resolution")
			} else {
				var conflict *client.HTTPError
				if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
					t.Fatalf("second resolution error=%v", err)
				}
			}
			for _, name := range queueNames {
				if err := queue.QueueResume(ctx, name, nil); err != nil {
					t.Fatal(err)
				}
			}
			final := awaitTerminal(t, ctx, api, admitted.Run.ID)
			var attempts, models, operations, completed, slots, open, successes int
			var finalRootDeadline, finalNodeDeadline, finalRequestDeadline time.Time
			var decision, evidence, operationID string
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND completed),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
				(SELECT execution_deadline FROM knotra_runs WHERE id=$1),
				(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2),
				(q.document->>'deadline')::timestamptz,q.response_id,q.response->>'decision',q.response->>'evidence',q.response->>'operationId',
				(SELECT count(*) FROM knotra_commands WHERE id=$4)
				FROM knotra_requests q WHERE q.id=$3`, final.ID, b, request.ID, key).Scan(&attempts, &models, &operations, &completed, &slots, &open, &successes, &finalRootDeadline, &finalNodeDeadline, &finalRequestDeadline, &responseID, &decision, &evidence, &operationID, &receipts); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantDecision := "succeeded", "completed"
			wantAttempts, wantCompleted, wantSuccesses := 3, 1, 3
			wantBCalls, wantCCalls := int32(1), int32(1)
			if outcome == "failed" {
				wantStatus, wantDecision = "failed", "failed"
				wantAttempts, wantCompleted, wantSuccesses, wantCCalls = 2, 0, 1, 0
			}
			if outcome == "not_started" {
				wantDecision, wantAttempts, wantCompleted, wantBCalls = "not_executed", 4, 2, 2
			}
			var preservedState, preservedAnswer string
			if err := admin.Pool.QueryRow(ctx, "SELECT state,outputs->'answer'->>'json' FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", final.ID, a).Scan(&preservedState, &preservedAnswer); err != nil || preservedState != "succeeded" || preservedAnswer != "42" {
				t.Fatalf("confirmed sibling changed: state=%s answer=%s error=%v", preservedState, preservedAnswer, err)
			}
			if outcome != "failed" && (string(final.Outputs["a"]) != "42" || string(final.Outputs["b"]) != "42" || string(final.Outputs["c"]) != "44") {
				t.Fatalf("resolution exports=%+v", final.Outputs)
			}
			if outcome == "failed" && (len(final.Outputs) != 0 || len(final.Artifacts) != 0) {
				t.Fatal("failed resolution published root outputs")
			}
			wantBGenerations := int32(1)
			if outcome == "failed" {
				wantBGenerations = 0
			}
			if final.Status != wantStatus || attempts != wantAttempts || models != wantAttempts || operations != wantAttempts || completed != wantCompleted || slots != 0 || open != 0 || successes != wantSuccesses || calls[0].Load() != 1 || calls[1].Load() != wantBCalls || calls[2].Load() != wantCCalls || generations[0].Load() != 1 || generations[1].Load() != wantBGenerations || generations[2].Load() != wantCCalls || responseID == nil || *responseID != key || decision != wantDecision || evidence != payload["evidence"] || operationID != request.Failure.OperationID || receipts != 1 || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) || !request.Deadline.Equal(finalRequestDeadline) {
				t.Fatalf("status=%s attempts=%d models=%d operations=%d/%d slots=%d open=%d successes=%d calls=%d/%d/%d decision=%s deadlines=%v/%v/%v", final.Status, attempts, models, completed, operations, slots, open, successes, calls[0].Load(), calls[1].Load(), calls[2].Load(), decision, rootDeadline.Equal(finalRootDeadline), nodeDeadline.Equal(finalNodeDeadline), request.Deadline.Equal(finalRequestDeadline))
			}
			t.Log("SIGKILL kept decision/receipt/node transition/root stop or resume atomic; confirmed sibling and original deadlines survived, with physical requests and generations matching the operator decision")
		})
	}
}

const riverResolutionRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: resolution-commit-recovery}
spec:
  limits: {timeout: 3m, maxConcurrentNodes: 2}
  models:
    writer: {connection: cloud, requires: [structuredOutput]}
  nodes:
    a:
      type: llm
      execution: {timeout: 2m, retry: {maxAttempts: 3}}
      llm: {model: writer, prompt: {text: fixture-a}}
      outputs: {answer: {schema: {type: integer}}}
    b:
      type: llm
      execution: {timeout: 2m, retry: {maxAttempts: 3}}
      llm: {model: writer, prompt: {text: fixture-b}}
      outputs: {answer: {schema: {type: integer}}}
    c:
      type: llm
      needs: [a, b]
      llm: {model: writer, prompt: {text: fixture-c}}
      outputs: {answer: {schema: {type: integer}}}
  outputs:
    a: {schema: {type: integer}, bind: {from: nodes.a.outputs.answer}}
    b: {schema: {type: integer}, bind: {from: nodes.b.outputs.answer}}
    c: {schema: {type: integer}, bind: {from: nodes.c.outputs.answer}}
`

const riverHumanRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: human-commit-recovery}
spec:
  limits: {timeout: 3m}
  nodes:
    review:
      type: foreach
      execution: {timeout: 2m}
      inputs:
        items: {schema: {type: array, items: {type: integer}}, bind: {value: [42]}}
      foreach:
        over: items
        concurrency: 1
        with: {item: {from: iteration.item}}
        body:
          inputs: {item: {schema: {type: integer}}}
          nodes:
            approve:
              type: human
              execution: {timeout: %s}
              inputs: {item: {schema: {type: integer}, bind: {from: inputs.item}}}
              human: {prompt: {text: Answer this item}}
              outputs: {answer: {schema: {type: string}}}
          outputs: {answer: {schema: {type: string}, bind: {from: nodes.approve.outputs.answer}}}
  outputs:
    answers: {schema: {type: array, items: {type: string}}, bind: {from: nodes.review.outputs.answer}}
`
