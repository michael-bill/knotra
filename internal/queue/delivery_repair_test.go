package queue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func deliveryPlan(endpoint string) contract.Plan {
	plan := runtimePlan(endpoint)
	node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
	export := node.Outputs["answer"]
	export.Bind = &contract.Binding{From: "nodes.a.outputs.answer"}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"a": node}, Outputs: map[string]contract.Port{"result": export}}
	return plan
}

func TestRuntimeCoalescesAdvanceBurstWithoutLosingWork(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"once\"}"},"done":true,"done_reason":"stop"}`)
	}))
	defer server.Close()
	id := admitRuntimeRun(t, r, deliveryPlan(server.URL))
	errs := make(chan error, 8)
	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			for range 10 {
				if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
					_, err := store.WakeExecution(ctx, tx, id, r.Wake)
					return err
				}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	writers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Maintenance must recognize the older unconsumed delivery as carrying
	// all 80 committed changes, rather than continually advancing the counter.
	for range 2 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var generation, applied int64
	var jobs int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT wake_generation,applied_generation,
		(SELECT count(*) FROM river_job WHERE kind='knotra_advance_v1' AND args->>'runId'=$1)
		FROM knotra_runs WHERE id=$1`, id).Scan(&generation, &applied, &jobs); err != nil {
		t.Fatal(err)
	}
	if generation != 81 || applied != 0 || jobs != 1 {
		t.Fatalf("wake burst: generation=%d applied=%d queued advances=%d", generation, applied, jobs)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `"once"` || calls.Load() != 1 {
		t.Fatalf("coalesced wake lost or repeated work: run=%+v calls=%d error=%v", run, calls.Load(), err)
	}
}

func consumeTestWakes(t *testing.T, r *Runtime, plan contract.Plan, id string) {
	t.Helper()
	for range 10 {
		applied := false
		if err := pgx.BeginFunc(context.Background(), r.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(context.Background(), tx, id)
			if err != nil {
				return err
			}
			applied, err = store.ApplyWake(context.Background(), tx, id, run.WakeGeneration)
			if err != nil || !applied {
				return err
			}
			return r.advanceTx(context.Background(), tx, plan, id, "")
		}); err != nil {
			t.Fatal(err)
		}
		if !applied {
			return
		}
	}
	t.Fatal("test scheduler failed to settle its continuations")
}

func TestRuntimeRepairsMissingDeliveryWithSameBusinessAttempt(t *testing.T) {
	for _, kind := range []string{"advance", "execute"} {
		states := []string{"deleted", "discarded", "completed", "cancelled", "running_expired", "healthy"}
		if kind == "advance" {
			states = append(states, "future_backoff", "malformed_args", "wrong_run", "future_generation")
		}
		for _, state := range states {
			t.Run(kind+"/"+state, func(t *testing.T) {
				r := newTestRuntime(t)
				ctx := context.Background()
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"once\"}"},"done":true,"done_reason":"stop"}`)
				}))
				defer server.Close()
				plan := deliveryPlan(server.URL)
				id := admitRuntimeRun(t, r, plan)
				if kind == "execute" {
					consumeTestWakes(t, r, plan, id)
				}
				jobKind := (AdvanceArgs{}).Kind()
				if kind == "execute" {
					jobKind = (ExecuteArgs{}).Kind()
				}
				switch state {
				case "deleted":
					if _, err := r.Store.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind=$1", jobKind); err != nil {
						t.Fatal(err)
					}
				case "running_expired":
					if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state='running',attempted_at=clock_timestamp()-interval '2 hours' WHERE kind=$1", jobKind); err != nil {
						t.Fatal(err)
					}
				case "future_backoff":
					if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state='retryable',scheduled_at=clock_timestamp()+interval '1 day' WHERE kind=$1", jobKind); err != nil {
						t.Fatal(err)
					}
				case "malformed_args", "wrong_run", "future_generation":
					field, value := "wakeGeneration", `"invalid"`
					if state == "wrong_run" {
						field, value = "runId", `"another-run"`
					}
					if state == "future_generation" {
						value = "1000"
					}
					if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET args=jsonb_set(args,ARRAY[$2]::text[],$3::jsonb) WHERE kind=$1", jobKind, field, value); err != nil {
						t.Fatal(err)
					}
				case "healthy":
				default:
					if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state=$2::river_job_state,finalized_at=clock_timestamp() WHERE kind=$1", jobKind, state); err != nil {
						t.Fatal(err)
					}
				}
				var before int64
				if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&before); err != nil {
					t.Fatal(err)
				}
				// Separate process-local scan cursors, shared domain lock/queue.
				var workers sync.WaitGroup
				errs := make(chan error, 4)
				for range 4 {
					workers.Go(func() { clone := &Runtime{Store: r.Store, Client: r.Client}; errs <- clone.reconcileDeliveries(ctx) })
				}
				workers.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				var after int64
				if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&after); err != nil {
					t.Fatal(err)
				}
				want := before + 1
				if state == "healthy" {
					want = before
				}
				if after != want {
					t.Fatalf("repaired wake generations=%d -> %d want=%d", before, after, want)
				}
				if err := r.Start(ctx); err != nil {
					t.Fatal(err)
				}
				await(t, 8*time.Second, func() (bool, error) {
					status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
					return protocol.Terminal(status), err
				})
				run, err := r.Store.Run(ctx, id)
				var attempts, number, debits int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT count(*),max(number),(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model')
					FROM knotra_execution_attempts WHERE run_id=$1`, id).Scan(&attempts, &number, &debits); err != nil {
					t.Fatal(err)
				}
				if err != nil || run.Status != "succeeded" || calls.Load() != 1 || attempts != 1 || number != 1 || debits != 1 {
					t.Fatalf("run=%+v calls=%d attempts=%d number=%d debits=%d error=%v", run, calls.Load(), attempts, number, debits, err)
				}
			})
		}
	}
}

func TestRuntimeDeliveryScanPassesHealthyJobsAndHonorsCapacityAndPause(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := deliveryPlan("")
	document := plan.Pipelines[plan.Root]
	leaf := document.Spec.Nodes["a"]
	document.Spec.Nodes = map[string]contract.Node{}
	document.Spec.Outputs = nil
	for i := range 80 {
		document.Spec.Nodes[fmt.Sprintf("work-%02d", i)] = leaf
	}
	id := admitRuntimeRun(t, r, plan)
	consumeTestWakes(t, r, plan, id)
	var last string
	if err := r.Store.Pool.QueryRow(ctx, "SELECT max(instance_id) FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind=$1 AND args->'attempt'->>'instanceId'=$2", (ExecuteArgs{}).Kind(), last); err != nil {
		t.Fatal(err)
	}
	generation := func() int64 {
		t.Helper()
		var value int64
		if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := generation()
	if err := r.reconcileDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	if generation() != before {
		t.Fatal("first scan did not respect its 64-attempt bound")
	}
	for range 2 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if generation() != before+1 {
		t.Fatal("healthy jobs hid a later missing delivery")
	}
	consumeTestWakes(t, r, plan, id)
	var attempts, number, dispatch int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT count(*),max(number),
		(SELECT dispatch_generation FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2)
		FROM knotra_execution_attempts WHERE run_id=$1`, id, last).Scan(&attempts, &number, &dispatch); err != nil || attempts != 80 || number != 1 || dispatch != 2 {
		t.Fatalf("attempts=%d number=%d dispatch=%d error=%v", attempts, number, dispatch, err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind=$1 AND args->'attempt'->>'instanceId'=$2", (ExecuteArgs{}).Kind(), last); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_scopes SET active_attempts=1 WHERE run_id=$1 AND id=$1", id); err != nil {
		t.Fatal(err)
	}
	before = generation()
	for range 3 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if generation() != before {
		t.Fatal("capacity miss created repeated deliveries")
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_scopes SET active_attempts=0 WHERE run_id=$1 AND id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET admission_paused=true WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if generation() != before {
		t.Fatal("pause admitted a fresh delivery")
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET admission_paused=false WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if generation() != before+1 {
		t.Fatal("restored capacity/admission did not repair the pending delivery")
	}
	consumeTestWakes(t, r, plan, id)
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE knotra_execution_attempts SET dispatch_pending=true WHERE run_id=$1", id); err != nil {
			return err
		}
		_, err := store.WakeExecution(ctx, tx, id, r.Wake)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		run, err := store.LockExecutionRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := store.ApplyWake(ctx, tx, id, run.WakeGeneration); err != nil {
			return err
		}
		return r.advanceTx(ctx, tx, plan, id, "")
	}); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND dispatch_pending", id).Scan(&pending); err != nil || pending != 16 {
		t.Fatalf("bounded redispatch left %d pending want=16 error=%v", pending, err)
	}
	consumeTestWakes(t, r, plan, id)
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND dispatch_pending", id).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("redispatch continuation left %d pending error=%v", pending, err)
	}

}

func TestRuntimeDeliveryRepairRollsBackRedispatchMarkerOnQueueFailure(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := deliveryPlan("")
	id := admitRuntimeRun(t, r, plan)
	consumeTestWakes(t, r, plan, id)
	if _, err := r.Store.Pool.Exec(ctx, "DELETE FROM river_job WHERE kind=$1", (ExecuteArgs{}).Kind()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, `CREATE FUNCTION reject_repair() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.kind='knotra_advance_v1' THEN RAISE EXCEPTION 'repair delivery failed'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_repair BEFORE INSERT ON river_job FOR EACH ROW EXECUTE FUNCTION reject_repair()`); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDeliveries(ctx); err == nil {
		t.Fatal("queue failure was not returned")
	}
	var after int64
	var pending bool
	if err := r.Store.Pool.QueryRow(ctx, `SELECT r.wake_generation,a.dispatch_pending FROM knotra_runs r
		JOIN knotra_execution_attempts a ON a.run_id=r.id WHERE r.id=$1`, id).Scan(&after, &pending); err != nil || after != before || pending {
		t.Fatalf("repair escaped rollback: generation=%d/%d pending=%v error=%v", after, before, pending, err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER reject_repair ON river_job"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.reconcileDeliveries(ctx); err != nil {
			t.Fatal(err)
		}
	}
	consumeTestWakes(t, r, plan, id)
	var generation int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT dispatch_generation FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("redispatch generation=%d error=%v", generation, err)
	}
}

func TestRuntimeDeliveryTracksWorkerHeartbeatBeforeClaim(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	id := admitRuntimeRun(t, r, deliveryPlan(""))
	consumeTestWakes(t, r, deliveryPlan(""), id)
	if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.BindWorkerDeliveryClient(ctx, r.WorkerID, r.Client.River.ID()); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.BindWorkerDeliveryClient(ctx, r.WorkerID, "different-client"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rebinding=%v", err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET state='running',attempted_at=clock_timestamp(),attempted_by=ARRAY[$1] WHERE kind=$2", r.Client.River.ID(), (ExecuteArgs{}).Kind()); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			attempts, err := store.ReadExecutionAttempts(ctx, tx, id, execution.StableID("g", id+"/root"), nil)
			if err != nil {
				return err
			}
			ok, err := r.Client.hasDelivery(ctx, tx, ExecuteArgs{Attempt: attempts[0].AttemptID, DispatchGeneration: attempts[0].DispatchGeneration, RoutingVersion: RoutingVersion}, time.Now())
			if err == nil && ok != want {
				return fmt.Errorf("delivery viable=%v want=%v", ok, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(true)
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1", r.WorkerID); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp() WHERE id=$1", r.WorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET attempted_at=clock_timestamp()-interval '2 hours' WHERE kind=$1", (ExecuteArgs{}).Kind()); err != nil {
		t.Fatal(err)
	}
	check(false)
}
