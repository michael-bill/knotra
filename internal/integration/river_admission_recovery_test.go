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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

func TestRiverAdmissionProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for admission SIGKILL checks")
	}
	t.Setenv("KNOTRA_ADMISSION_FIXTURE_KEY", "fixture-key")
	for _, mode := range []string{"before_delivery_insert", "before_commit", "committed_before_receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			var calls atomic.Int32
			marker := strings.Repeat("frozen-input-", 12500)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				calls.Add(1)
				body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil || !strings.Contains(string(body), marker) || request.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("provider received changed/truncated input or credentials: bytes=%d error=%v", len(body), err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"knotra_output","arguments":"{\"answer\":42}"}]}`)
			}))
			defer provider.Close()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_ADMISSION_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			first := startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "before.log"), api)
			identity := engineIdentity(t, ctx, api)
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
			queues := []string{"knotra_advance_" + identity, "knotra_execute_" + identity}
			for _, name := range queues {
				if err := queue.QueuePause(ctx, name, nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var definition struct{ Definition protocol.Definition }
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: riverAdmissionRecoveryPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(riverAdmissionRecoveryPipeline)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			commands, received, release := commandReceiptProxy(t, ctx, api, "/v1/runs", filepath.Join(work, "admission-client"), mode == "committed_before_receipt")
			key := uuid.NewString()
			payload := map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name, "inputs": map[string]any{"marker": marker}}
			var barrier *pgx.Conn
			var table string
			var barrierPID uint32
			if mode != "committed_before_receipt" {
				barrier, err = pgx.Connect(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				barrierPID = barrier.PgConn().PID()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273656)"); err != nil {
					t.Fatal(err)
				}
				table = "knotra_commands"
				when := "NEW.id='" + key + "'"
				if mode == "before_delivery_insert" {
					table, when = "river_job", "NEW.kind IN ('knotra_advance_v1','knotra_execute_v1')"
				}
				if _, err := admin.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION admission_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273656); RETURN NEW; END $$; CREATE TRIGGER admission_crash_barrier BEFORE INSERT ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION admission_crash_barrier()`, table, when)); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- commands.Command(ctx, "/runs", payload, key, new(any)) }()
			var committedReceipt []byte
			if barrier != nil {
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrierPID)).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("admission exited before its commit barrier: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-ticker.C:
					}
				}
			} else {
				select {
				case response := <-received:
					if response.status != http.StatusCreated {
						t.Fatalf("held admission receipt=%d %s", response.status, response.data)
					}
					committedReceipt = response.data
				case err := <-done:
					t.Fatalf("admission exited before the receipt was withheld: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			checkAdmission := func(want int) {
				t.Helper()
				var runs, graphs, scopes, nodes, attempts, receipts, advances, executes, effects, budgets, slots, outbox int
				if err := admin.Pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM knotra_runs),(SELECT count(*) FROM knotra_execution_graphs),
					(SELECT count(*) FROM knotra_execution_scopes),(SELECT count(*) FROM knotra_execution_nodes),
					(SELECT count(*) FROM knotra_execution_attempts WHERE state='ready' AND owner IS NULL AND number=1),
					(SELECT count(*) FROM knotra_commands WHERE id=$1),
					(SELECT count(*) FROM river_job WHERE kind='knotra_advance_v1'),
					(SELECT count(*) FROM river_job WHERE kind='knotra_execute_v1'),
					(SELECT count(*) FROM knotra_operations),COALESCE((SELECT sum(used) FROM knotra_budgets),0),
					COALESCE((SELECT sum(active_attempts) FROM knotra_execution_scopes),0),
					(SELECT count(*) FROM knotra_outbox)`, key).Scan(&runs, &graphs, &scopes, &nodes, &attempts, &receipts, &advances, &executes, &effects, &budgets, &slots, &outbox); err != nil {
					t.Fatal(err)
				}
				if runs != want || graphs != want || scopes != want || nodes != want || attempts != want || receipts != want || advances != want || executes != want || effects != 0 || budgets != 0 || slots != 0 || outbox != 0 || calls.Load() != 0 {
					t.Fatalf("admission run/graph/scope/node/attempt/receipt/jobs=%d/%d/%d/%d/%d/%d/%d/%d want=%d effects/budgets/slots/outbox/calls=%d/%d/%d/%d/%d", runs, graphs, scopes, nodes, attempts, receipts, advances, executes, want, effects, budgets, slots, outbox, calls.Load())
				}
			}
			want := 0
			if mode == "committed_before_receipt" {
				want = 1
			}
			checkAdmission(want)
			var originalRootDeadline, originalNodeDeadline time.Time
			if want == 1 {
				if err := admin.Pool.QueryRow(ctx, "SELECT execution_deadline,(SELECT deadline FROM knotra_execution_nodes) FROM knotra_runs").Scan(&originalRootDeadline, &originalNodeDeadline); err != nil {
					t.Fatal(err)
				}
			}
			first.kill(t)
			if state, ok := first.command.ProcessState.Sys().(syscall.WaitStatus); !ok || state.Signal() != syscall.SIGKILL {
				t.Fatalf("engine did not exit from SIGKILL: %v", first.command.ProcessState)
			}
			release()
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273656)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER admission_crash_barrier ON "+table); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("client received an admission confirmation before process loss")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			pending, err := commands.Commands()
			if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].Status != "pending" {
				t.Fatalf("client lost its pending admission: %+v %v", pending, err)
			}
			checkAdmission(want)
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != identity {
				t.Fatal("engine identity changed during admission recovery")
			}
			var replay json.RawMessage
			if err := commands.Retry(ctx, key, &replay); err != nil {
				t.Fatal(err)
			}
			if want == 1 && string(replay) != string(committedReceipt) {
				t.Fatal("admission retry replaced the committed receipt")
			}
			var accepted struct{ Run protocol.Run }
			if err := json.Unmarshal(replay, &accepted); err != nil || accepted.Run.ID == "" {
				t.Fatalf("invalid admission receipt: %s %v", replay, err)
			}
			var duplicate json.RawMessage
			if err := api.Command(ctx, "/runs", payload, key, &duplicate); err != nil || string(duplicate) != string(replay) {
				t.Fatalf("fresh client did not replay the same admission: %v", err)
			}
			checkAdmission(1)
			var admitted, rootDeadline, nodeDeadline time.Time
			var backend string
			if err := admin.Pool.QueryRow(ctx, "SELECT admitted_at,execution_deadline,backend,(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1) FROM knotra_runs WHERE id=$1", accepted.Run.ID).Scan(&admitted, &rootDeadline, &backend, &nodeDeadline); err != nil {
				t.Fatal(err)
			}
			if backend != execution.BackendRiver || !accepted.Run.CreatedAt.Equal(admitted) || !rootDeadline.Equal(admitted.Add(3*time.Minute)) || (want == 1 && (!rootDeadline.Equal(originalRootDeadline) || !nodeDeadline.Equal(originalNodeDeadline))) {
				t.Fatalf("admitted identity/clock/deadlines changed: backend=%s created=%v admitted=%v root=%v node=%v", backend, accepted.Run.CreatedAt, admitted, rootDeadline, nodeDeadline)
			}
			for _, name := range queues {
				if err := queue.QueueResume(ctx, name, nil); err != nil {
					t.Fatal(err)
				}
			}
			final := awaitTerminal(t, ctx, api, accepted.Run.ID)
			var exported string
			if err := json.Unmarshal(final.Outputs["marker"], &exported); err != nil || exported != marker || string(final.Outputs["answer"]) != "42" || final.Status != "succeeded" {
				t.Fatalf("recovered run=%s marker bytes=%d diagnostics=%+v error=%v", final.Status, len(exported), final.Diagnostics, err)
			}
			var attempts, operations, debits, slots, successes, receipts int
			var finalRootDeadline, finalNodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='model' AND completed),
				(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
				(SELECT count(*) FROM knotra_commands WHERE id=$2),
				r.execution_deadline,n.deadline FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id WHERE r.id=$1`, final.ID, key).Scan(&attempts, &operations, &debits, &slots, &successes, &receipts, &finalRootDeadline, &finalNodeDeadline); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || operations != 1 || debits != 1 || slots != 0 || successes != 1 || receipts != 1 || calls.Load() != 1 || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) {
				t.Fatalf("attempts/operations/debits/slots/events/receipts/calls=%d/%d/%d/%d/%d/%d/%d deadlines=%v/%v", attempts, operations, debits, slots, successes, receipts, calls.Load(), rootDeadline.Equal(finalRootDeadline), nodeDeadline.Equal(finalNodeDeadline))
			}
			other := &client.Client{BaseURL: api.BaseURL, StateDir: filepath.Join(work, "different-client")}
			if err := other.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name, "inputs": map[string]any{"marker": "different"}}, key, new(any)); err == nil {
				t.Fatal("a reused command key admitted different input")
			} else {
				var conflict *client.HTTPError
				if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
					t.Fatalf("changed admission error=%v", err)
				}
			}
			var runs int
			if err := admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_runs").Scan(&runs); err != nil || runs != 1 || calls.Load() != 1 {
				t.Fatalf("conflicting admission created work: runs=%d calls=%d error=%v", runs, calls.Load(), err)
			}
			t.Log("SIGKILL kept admission, first deliveries and receipt atomic; client retry preserved one run/attempt/call/debit, full input/export and original deadlines")
		})
	}
}

const riverAdmissionRecoveryPipeline = `apiVersion: knotra/v1
kind: Pipeline
metadata: {name: admission-recovery}
spec:
  limits: {timeout: 3m}
  inputs:
    marker: {schema: {type: string}}
  models:
    writer: {connection: cloud, requires: [structuredOutput]}
  nodes:
    work:
      type: llm
      execution: {timeout: 30s}
      inputs:
        marker: {schema: {type: string}, bind: {from: inputs.marker}}
      llm: {model: writer, prompt: {text: Return answer 42.}}
      outputs:
        answer: {schema: {type: integer}}
  outputs:
    marker: {schema: {type: string}, bind: {from: inputs.marker}}
    answer: {schema: {type: integer}, bind: {from: nodes.work.outputs.answer}}
`
