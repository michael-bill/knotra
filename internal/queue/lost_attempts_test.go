package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRuntimeLostAttemptRequiresEvidenceAndRetainsLateOutcome(t *testing.T) {
	for _, mode := range []string{"journaled_response", "corrupt_envelope", "saved_envelope", "enqueue_rollback", "unconfirmed_operation", "agent_prefix", "fail_policy", "cancel", "node_deadline", "run_deadline"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx := context.Background()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"received\"}"},"done":true,"done_reason":"stop"}`)
			}))
			defer server.Close()
			plan := runtimePlan(server.URL)
			node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
			node.Execution.Retry = &contract.Retry{MaxAttempts: 5, Backoff: "1ms"}
			if mode == "agent_prefix" {
				node.Type, node.LLM, node.Agent = "agent", nil, &contract.AgentNode{Model: "model", MaxSteps: 5}
			}
			if mode == "fail_policy" {
				node.Execution.OnUnknownOutcome = "fail"
			}
			export := node.Outputs["answer"]
			export.Bind = &contract.Binding{From: "nodes.a.outputs.answer"}
			plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"a": node}, Outputs: map[string]contract.Port{"result": export}}
			id := admitRuntimeRun(t, r, plan)
			if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID); err != nil {
				t.Fatal(err)
			}
			var claim *execution.Claim
			if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
				if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
					return err
				}
				var err error
				claim, err = r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/a"), Number: 1}, 1, r.WorkerID)
				return err
			}); err != nil || claim == nil {
				t.Fatalf("claim=%+v error=%v", claim, err)
			}
			var result execution.ExecuteResult
			operation := ""
			switch mode {
			case "unconfirmed_operation":
				// The provider really receives its request. Response journaling then
				// fails, and the process loses even its classified final result.
				if _, err := r.Store.Pool.Exec(ctx, `CREATE FUNCTION lose_response() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN IF NEW.completed AND NOT OLD.completed THEN RAISE EXCEPTION 'lost response commit'; END IF; RETURN NEW; END $$;
					CREATE TRIGGER lose_response BEFORE UPDATE ON knotra_operations FOR EACH ROW EXECUTE FUNCTION lose_response()`); err != nil {
					t.Fatal(err)
				}
				result = r.Host.Execute(ctx, claim.Request)
				if result.Failure == nil || !result.Failure.Unknown {
					t.Fatalf("received response was not classified unknown: %+v", result)
				}
				operation = result.Failure.OperationID
			case "agent_prefix":
				operation = "lost-agent-step"
				if _, err := r.Store.BeginOwnedOperation(ctx, claim.Ownership, "completed-agent-prefix", "", "write"); err != nil {
					t.Fatal(err)
				}
				if err := r.Store.CompleteOwnedOperation(ctx, claim.Ownership, "completed-agent-prefix", json.RawMessage(`{"saved":"prefix"}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.BeginOwnedOperation(ctx, claim.Ownership, operation, "", "write"); err != nil {
					t.Fatal(err)
				}
				result = execution.ExecuteResult{Failure: &execution.Failure{Code: "OUTCOME_UNKNOWN", Unknown: true, OperationID: operation}}
			default:
				result = r.Host.Execute(ctx, claim.Request)
				if result.Failure != nil {
					t.Fatal(result.Failure)
				}
			}
			if err := r.reconcileOutcomes(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := r.Store.Pool.QueryRow(ctx, "SELECT state FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&state); err != nil || state != "claimed" {
				t.Fatalf("live claim changed: %s %v", state, err)
			}
			if mode == "cancel" {
				srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
				if w := runtimeCommand(t, srv.Handler(), "/v1/runs/"+id+"/cancel", "cancel", `{}`); w.Code != 202 {
					t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
				}
				run, err := r.Store.Run(ctx, id)
				if err != nil || run.Status == "cancelled" {
					t.Fatalf("run terminated before active attempt reconciliation: %+v %v", run, err)
				}
			}
			if mode == "node_deadline" {
				if _, err := r.Store.Pool.Exec(ctx, `UPDATE knotra_execution_nodes SET deadline=clock_timestamp()-interval '1 second',
					execution_request=jsonb_set(execution_request,'{deadline}',to_jsonb(clock_timestamp()-interval '1 second')) WHERE run_id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "run_deadline" {
				if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET execution_deadline=clock_timestamp()-interval '1 second' WHERE id=$1", id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", id); err != nil {
				t.Fatal(err)
			}
			corruptPath := ""
			if mode == "corrupt_envelope" {
				directory := t.TempDir()
				files, err := execution.OpenOutcomeFiles(directory)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = files.Close() }()
				r.Outcomes = files
				key, err := files.Put(execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: plan.Digest, CompletedAt: time.Now(), Outputs: result.Outputs})
				if err != nil {
					t.Fatal(err)
				}
				corruptPath = filepath.Join(directory, key+".json")
				if err := os.WriteFile(corruptPath, []byte("corrupt durable evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "saved_envelope" {
				outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: plan.Digest, CompletedAt: time.Now(), Outputs: result.Outputs}
				if _, err := r.Outcomes.Put(outcome); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := r.reconcileOutcomes(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.Client.Start(ctx); err != nil {
					t.Fatal(err)
				}
				await(t, 5*time.Second, func() (bool, error) { run, err := r.Store.Run(ctx, id); return run.Status == "succeeded", err })
				var slots, requests int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT s.active_attempts,(SELECT count(*) FROM knotra_requests WHERE run_id=$1)
                    FROM knotra_execution_scopes s WHERE s.run_id=$1 AND s.id=$1`, id).Scan(&slots, &requests); err != nil || slots != 0 || requests != 0 || calls.Load() != 1 {
					t.Fatalf("saved envelope recovery slots=%d requests=%d calls=%d error=%v", slots, requests, calls.Load(), err)
				}
				return
			}
			if mode == "enqueue_rollback" {
				// Force a new wake insertion; an existing viable wake can otherwise
				// absorb the generation and bypass this deliberate INSERT failure.
				if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state='discarded',finalized_at=clock_timestamp() WHERE kind='knotra_advance_v1'"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.Pool.Exec(ctx, `CREATE FUNCTION reject_lost_wake() RETURNS trigger LANGUAGE plpgsql AS $$
                    BEGIN IF NEW.kind='knotra_advance_v1' THEN RAISE EXCEPTION 'advance delivery rejected'; END IF; RETURN NEW; END $$;
                    CREATE TRIGGER reject_lost_wake BEFORE INSERT ON river_job FOR EACH ROW EXECUTE FUNCTION reject_lost_wake()`); err != nil {
					t.Fatal(err)
				}
				r.outcomeCursor = ""
				if err := r.reconcileOutcomes(ctx); err == nil {
					t.Fatal("lost-attempt transaction committed without its delivery")
				}
				var slots, requests int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT a.state,s.active_attempts,
                    (SELECT count(*) FROM knotra_requests WHERE run_id=$1)
                    FROM knotra_execution_attempts a JOIN knotra_execution_scopes s ON s.run_id=a.run_id AND s.id=a.run_id
                    WHERE a.run_id=$1`, id).Scan(&state, &slots, &requests); err != nil || state != "claimed" || slots != 1 || requests != 0 {
					t.Fatalf("lost transition escaped rollback: state=%s slots=%d requests=%d error=%v", state, slots, requests, err)
				}
				if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER reject_lost_wake ON river_job"); err != nil {
					t.Fatal(err)
				}
			}
			// Concurrent independent reconcilers cannot duplicate the request,
			// projection or release the same scope reservation twice.
			errs := make(chan error, 4)
			var workers sync.WaitGroup
			for range 4 {
				workers.Go(func() {
					clone := &Runtime{Store: r.Store, Host: r.Host, Outcomes: r.Outcomes, Client: r.Client}
					errs <- clone.reconcileOutcomes(ctx)
				})
			}
			workers.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileOutcomes(ctx); err != nil {
				t.Fatal(err)
			}
			var slots, attempts, requests int
			if err := r.Store.Pool.QueryRow(ctx, `SELECT
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open')`, id).Scan(&slots, &attempts, &requests); err != nil || slots != 0 || attempts != 1 {
				t.Fatalf("slots=%d attempts=%d requests=%d error=%v", slots, attempts, requests, err)
			}
			if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { _, err := r.Store.RenewAttempt(ctx, tx, claim.Ownership); return err }); !errors.Is(err, store.ErrExecutionOwnership) {
				t.Fatalf("lost owner renewed: %v", err)
			}
			outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: plan.Digest, CompletedAt: time.Now(), Outputs: result.Outputs, Failure: result.Failure}
			if mode == "corrupt_envelope" {
				if _, err := r.Outcomes.Put(outcome); !errors.Is(err, execution.ErrInvalidOutcome) {
					t.Fatalf("overwrote corrupt evidence: %v", err)
				}
			} else {
				if _, err := r.Outcomes.Put(outcome); err != nil {
					t.Fatal(err)
				}
				// The production scan must discover late evidence after fencing;
				// directly calling publish would hide a claimed-only scan bug.
				for range 3 {
					if err := r.reconcileOutcomes(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.Client.Start(ctx); err != nil {
					t.Fatal(err)
				}
				await(t, 5*time.Second, func() (bool, error) {
					var retained bool
					err := r.Store.Pool.QueryRow(ctx, "SELECT outcome IS NOT NULL FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&retained)
					return retained, err
				})
			}
			run, err := r.Store.Run(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			want := "waiting_resolution"
			switch mode {
			case "fail_policy", "node_deadline", "run_deadline":
				want = "failed"
			case "cancel":
				want = "cancelled"
			}
			if run.Status != want || len(run.Outputs) != 0 {
				t.Fatalf("lost/late outcome changed state: %+v", run)
			}
			if want == "waiting_resolution" {
				if requests != 1 {
					t.Fatalf("resolution requests=%d", requests)
				}
				requestID := execution.StableID("q", claim.InstanceID+"/resolution/1")
				request, err := r.Store.Request(ctx, requestID)
				if err != nil || request.Failure == nil || (operation != "" && request.Failure.OperationID != operation) {
					t.Fatalf("request=%+v error=%v", request, err)
				}
				if mode == "agent_prefix" && request.Failure.CanRetryIfNotExecuted {
					t.Fatal("lost agent prefix became restartable")
				}
				srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
				body := `{"outcome":"succeeded","evidence":"external result verified","outputs":{"answer":"confirmed"}}`
				if mode == "agent_prefix" {
					body = `{"outcome":"not_started","evidence":"unfinished suffix verified"}`
				}
				if w := runtimeCommand(t, srv.Handler(), "/v1/runs/"+id+"/instances/"+claim.InstanceID+"/resolve", "resolve", body); w.Code != 202 {
					t.Fatalf("resolution=%d %s", w.Code, w.Body.String())
				}
				run, err = r.Store.Run(ctx, id)
				want = "succeeded"
				if mode == "agent_prefix" {
					want = "failed"
				}
				if err != nil || run.Status != want {
					t.Fatalf("resolved run=%+v error=%v", run, err)
				}
			}
			if mode == "cancel" || mode == "node_deadline" || mode == "run_deadline" {
				found := false
				for _, diagnostic := range run.Diagnostics {
					found = found || diagnostic.Code == "OUTCOME_UNKNOWN"
				}
				if !found {
					t.Fatalf("unknown effect lost from diagnostics: %+v", run.Diagnostics)
				}
			}
			wantCalls := int32(1)
			if mode == "agent_prefix" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("lost attempt repeated a physical call: %d", calls.Load())
			}
			if corruptPath != "" {
				data, err := os.ReadFile(corruptPath)
				if err != nil || string(data) != "corrupt durable evidence" {
					t.Fatalf("corrupt evidence changed: %q %v", data, err)
				}
			}
			var evidence bool
			if err := r.Store.Pool.QueryRow(ctx, "SELECT outcome IS NOT NULL FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&evidence); err != nil || evidence == (mode == "corrupt_envelope") {
				t.Fatalf("verified evidence=%v error=%v", evidence, err)
			}
		})
	}
}

func TestRuntimeTimerPumpBoundsOverdueBatchAndRecoversRemainder(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	id := admitRuntimeRun(t, r, runtimePlan(""))
	if _, err := r.Store.Pool.Exec(ctx, `INSERT INTO knotra_execution_timers(run_id,id,generation,kind,due_at)
		SELECT $1,'overdue-'||n,1,'run_deadline',clock_timestamp()-interval '1 second' FROM generate_series(1,129) n`, id); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{64, 128, 129, 129} {
		if err := r.pumpTimers(ctx); err != nil {
			t.Fatal(err)
		}
		var consumed int
		if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_timers WHERE run_id=$1 AND consumed_at IS NOT NULL", id).Scan(&consumed); err != nil || consumed != want {
			t.Fatalf("consumed timers=%d want=%d error=%v", consumed, want, err)
		}
	}
	var wakes int64
	var jobs int
	// Batches advance domain generations, but one viable delivery consumes
	// all four committed generations under the run lock.
	if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation,(SELECT count(*) FROM river_job) FROM knotra_runs WHERE id=$1", id).Scan(&wakes, &jobs); err != nil || wakes != 4 || jobs != 1 {
		t.Fatalf("timer continuations wakes=%d jobs=%d error=%v", wakes, jobs, err)
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		run, err := store.LockExecutionRun(ctx, tx, id)
		if err != nil {
			return err
		}
		pending, err := r.Client.hasPendingAdvance(ctx, tx, run)
		if err != nil {
			return err
		}
		if !pending {
			return errors.New("timer continuation has no viable scheduler delivery")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
