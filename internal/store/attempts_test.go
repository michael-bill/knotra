package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func attemptFixture(t *testing.T) (*Store, []execution.AttemptID, []string, EnqueueWake) {
	t.Helper()
	s := testStore(t)
	run, plan, enqueue := executionAdmissionFixture(t, s)
	ctx := context.Background()
	var ids []execution.AttemptID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		run, err = PutRiverRun(ctx, tx, run, plan, contract.Values{}, enqueue)
		if err != nil {
			return err
		}
		state, err := LockExecutionRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		for _, graph := range []string{"a", "b"} {
			limits := plan.Profile.Spec.Limits
			limits.MaxConcurrentNodes = 1
			scopeID := "scope-" + graph
			b, err := json.Marshal(limits)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO knotra_execution_scopes(run_id,id,limits) VALUES($1,$2,$3)", run.ID, scopeID, b); err != nil {
				return err
			}
			scopes := []execution.BudgetScope{{ID: run.ID, Limits: plan.Profile.Spec.Limits}, {ID: scopeID, Limits: limits}}
			b, err = json.Marshal(scopes)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO knotra_execution_graphs(run_id,id,address,pipeline,graph_path,inputs,scopes,deadline,state)
				VALUES($1,$2,$2,'pipeline.yaml','root','{}',$3,$4,'running')`, run.ID, graph, b, state.Deadline); err != nil {
				return err
			}
			for i := range 3 {
				id := execution.AttemptID{RunID: run.ID, InstanceID: fmt.Sprintf("%s.n%d", graph, i), Number: 1}
				q := execution.ExecuteRequest{RunID: id.RunID, InstanceID: id.InstanceID, NodeID: id.InstanceID, Pipeline: "pipeline.yaml", Attempt: 1,
					Node: contract.Node{Type: "tool"}, Scopes: scopes, Deadline: state.Deadline}
				b, err := json.Marshal(q)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO knotra_execution_nodes(run_id,id,graph_id,node_id,address,state,execution_request,deadline,attempt_number)
					VALUES($1,$2,$3,$2,$2,'ready',$4,$5,1)`, run.ID, id.InstanceID, graph, b, state.Deadline); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO knotra_execution_attempts(run_id,instance_id,number,dispatch_generation,state)
					VALUES($1,$2,1,1,'ready')`, run.ID, id.InstanceID); err != nil {
					return err
				}
				ids = append(ids, id)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := []string{uuid.NewString(), uuid.NewString()}
	for _, worker := range workers {
		if err := s.RegisterWorker(ctx, worker, "test-host"); err != nil {
			t.Fatal(err)
		}
	}
	return s, ids, workers, enqueue
}

func claimFixtureAttempt(ctx context.Context, s *Store, id execution.AttemptID, generation int64, worker string) (*execution.Claim, error) {
	var claim *execution.Claim
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		claim, err = s.ClaimAttempt(ctx, tx, id, generation, worker)
		return err
	})
	return claim, err
}

func TestAttemptClaimsReserveAncestorCapacityAndReleaseOnce(t *testing.T) {
	s, ids, workers, enqueue := attemptFixture(t)
	ctx := context.Background()
	if claim, err := claimFixtureAttempt(ctx, s, ids[0], 2, workers[0]); err != nil || claim != nil {
		t.Fatalf("stale dispatch claimed work: %v %v", claim, err)
	}
	// A claim's savepoint does not commit the delivery transaction.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := s.ClaimAttempt(ctx, tx, ids[0], 1, workers[0]); err != nil || claim == nil {
		t.Fatalf("claim before rollback: %v %v", claim, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan *execution.Claim, 36)
	errs := make(chan error, 36)
	for i := range 36 {
		wg.Go(func() {
			claim, err := claimFixtureAttempt(ctx, s, ids[i%len(ids)], 1, workers[i%len(workers)])
			if err != nil {
				errs <- err
			} else if claim != nil {
				claims <- claim // A physical executor could start only after this commit.
			}
		})
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(claims) != 2 {
		t.Fatalf("expected one claim per child and two root slots; got %d", len(claims))
	}
	first := <-claims
	second := <-claims
	if first.Request.Scopes[1].ID == second.Request.Scopes[1].ID {
		t.Fatal("ancestor concurrency was bypassed")
	}
	key, err := first.Ownership.OutcomeKey()
	if err != nil || key != first.OutcomeKey || first.Request.Ownership == nil || *first.Request.Ownership != first.Ownership {
		t.Fatalf("claim did not bind outcome/request to ownership: %+v %v", first, err)
	}
	var rootActive, childActive, slots, ready, pending int
	if err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
		(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1 AND id<>$1),
		(SELECT count(*) FROM knotra_execution_slots WHERE run_id=$1 AND released_at IS NULL),
		(SELECT count(*) FROM knotra_execution_nodes WHERE run_id=$1 AND state='ready' AND attempt_number=1),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND state='ready' AND dispatch_pending)`, ids[0].RunID).
		Scan(&rootActive, &childActive, &slots, &ready, &pending); err != nil {
		t.Fatal(err)
	}
	if rootActive != 2 || childActive != 2 || slots != 4 || ready != 4 || pending != 4 {
		t.Fatalf("capacity miss mutated business work: root=%d child=%d slots=%d ready=%d pending=%d", rootActive, childActive, slots, ready, pending)
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := ReleaseAttemptSlots(ctx, tx, first.Ownership, enqueue); !errors.Is(err, ErrConflict) {
			return fmt.Errorf("released active execution: %w", err)
		}
		_, err := tx.Exec(ctx, "UPDATE knotra_execution_attempts SET state='completed' WHERE run_id=$1 AND instance_id=$2 AND number=$3", first.RunID, first.InstanceID, first.Number)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Even committing an outer transaction after an enqueue failure cannot lose
	// the slot release's wakeup or leave partially decremented counters.
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := ReleaseAttemptSlots(ctx, tx, first.Ownership, func(ctx context.Context, tx pgx.Tx, id string, generation int64) error {
			if err := enqueue(ctx, tx, id, generation); err != nil {
				return err
			}
			return errors.New("injected queue failure")
		})
		if err == nil {
			return errors.New("accepted queue failure")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			token := first.Ownership
			if i == 0 {
				token.Generation++
			}
			released, err := ReleaseAttemptSlots(ctx, tx, token, enqueue)
			if err != nil || released != (i == 1) {
				return fmt.Errorf("release %d: released=%v error=%w", i, released, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var wake, jobs int
	if err := s.Pool.QueryRow(ctx, `SELECT wake_generation,(SELECT count(*) FROM river_job),
		(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
		(SELECT count(*) FROM knotra_execution_slots WHERE run_id=$1 AND released_at IS NULL)
		FROM knotra_runs WHERE id=$1`, first.RunID).Scan(&wake, &jobs, &rootActive, &slots); err != nil {
		t.Fatal(err)
	}
	if wake != 2 || jobs != 2 || rootActive != 1 || slots != 2 {
		t.Fatalf("release was not atomic/idempotent: wake=%d jobs=%d root=%d slots=%d", wake, jobs, rootActive, slots)
	}
	var newlyClaimed int
	for _, id := range ids {
		if id == first.AttemptID || id == second.AttemptID {
			continue
		}
		claim, err := claimFixtureAttempt(ctx, s, id, 1, workers[0])
		if err != nil {
			t.Fatal(err)
		}
		if claim != nil {
			newlyClaimed++
		}
	}
	if newlyClaimed != 1 {
		t.Fatalf("release did not admit exactly one waiting attempt: %d", newlyClaimed)
	}
	// A delivered capacity miss must remain discoverable even if dispatch had
	// already cleared the pending marker when it inserted the delivery.
	for _, id := range ids {
		if _, err := s.Pool.Exec(ctx, `UPDATE knotra_execution_attempts SET dispatch_pending=false
			WHERE run_id=$1 AND instance_id=$2 AND state='ready'`, id.RunID, id.InstanceID); err != nil {
			t.Fatal(err)
		}
		if _, err := claimFixtureAttempt(ctx, s, id, 1, workers[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND state='ready' AND dispatch_pending`, first.RunID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 3 {
		t.Fatalf("delivered capacity misses were not marked for redispatch: %d", pending)
	}
}

func TestAttemptOwnershipRejectsExpiryCancellationAndStaleWorkers(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := context.Background()
	if err := s.RegisterWorker(ctx, workers[0], "test-host"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused worker incarnation: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_workers SET scheduler_versions=ARRAY[999] WHERE id=$1", workers[1]); err != nil {
		t.Fatal(err)
	}
	if claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[1]); err != nil || claim != nil {
		t.Fatalf("incompatible worker claimed: %+v %v", claim, err)
	}
	if err := s.DrainWorker(ctx, workers[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_workers SET scheduler_versions=ARRAY[$2]::integer[] WHERE id=$1", workers[1], execution.SchedulerVersion); err != nil {
		t.Fatal(err)
	}
	if claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[1]); err != nil || claim != nil {
		t.Fatalf("draining worker claimed: %+v %v", claim, err)
	}
	claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if err := s.HeartbeatWorker(ctx, workers[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainWorker(ctx, workers[0]); err != nil {
		t.Fatal(err)
	}
	// Draining and a root pause prevent new claims but let admitted work finish.
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_runs SET admission_paused=true WHERE id=$1", claim.RunID); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		expires, err := s.RenewAttempt(ctx, tx, claim.Ownership)
		if err == nil && !expires.After(claim.LeaseExpiresAt) {
			return errors.New("lease did not renew")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := claim.Ownership
	stale.Generation++
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := s.RenewAttempt(ctx, tx, stale)
		return err
	})
	if !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("stale ownership renewed: %v", err)
	}
	for _, mutation := range []string{
		"UPDATE knotra_runs SET cancel_requested=true WHERE id=$1",
		"UPDATE knotra_runs SET cancel_requested=false,stop_cause='{\"code\":\"EXECUTION_FAILED\"}' WHERE id=$1",
		"UPDATE knotra_runs SET stop_cause=NULL,execution_deadline=clock_timestamp()-interval '1 second' WHERE id=$1",
		"UPDATE knotra_runs SET execution_deadline=clock_timestamp()+interval '1 hour' WHERE id=$1; UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1",
	} {
		// Separate statements use pgx's simple protocol only for this SQL fixture.
		if _, err := s.Pool.Exec(ctx, mutation, pgx.QueryExecModeSimpleProtocol, claim.RunID); err != nil {
			t.Fatal(err)
		}
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			_, err := s.RenewAttempt(ctx, tx, claim.Ownership)
			return err
		})
		if !errors.Is(err, ErrExecutionOwnership) {
			t.Fatalf("ownership survived %s: %v", mutation, err)
		}
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_workers SET heartbeat_at=clock_timestamp()-interval '21 seconds' WHERE id=$1", workers[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.HeartbeatWorker(ctx, workers[0]); !errors.Is(err, ErrExecutionOwnership) {
		t.Fatalf("expired worker resurrected: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_workers SET draining=false WHERE id=$1", workers[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE knotra_runs SET admission_paused=false WHERE id=$1", claim.RunID); err != nil {
		t.Fatal(err)
	}
	if replay, err := claimFixtureAttempt(ctx, s, claim.AttemptID, 1, workers[0]); err != nil || replay != nil {
		t.Fatalf("expired attempt was executed again: %+v %v", replay, err)
	}
	if fresh, err := claimFixtureAttempt(ctx, s, ids[3], 1, workers[0]); err != nil || fresh != nil {
		t.Fatalf("expired worker claimed fresh work: %+v %v", fresh, err)
	}
}

func TestAttemptClaimRechecksDeadlineAfterLockWait(t *testing.T) {
	s, ids, workers, _ := attemptFixture(t)
	ctx := context.Background()
	owner, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Rollback(ctx) }()
	if _, err := LockExecutionRun(ctx, owner, ids[0].RunID); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	if err := owner.QueryRow(ctx, "UPDATE knotra_runs SET execution_deadline=clock_timestamp()+interval '100 milliseconds' WHERE id=$1 RETURNING execution_deadline", ids[0].RunID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(ready)
		claim, err := claimFixtureAttempt(ctx, s, ids[0], 1, workers[0])
		if err == nil && claim != nil {
			err = errors.New("lock wait extended the admission deadline")
		}
		done <- err
	}()
	<-ready
	time.Sleep(time.Until(deadline.Add(50 * time.Millisecond)))
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var slots int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_slots").Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 0 {
		t.Fatal("deadline miss reserved capacity")
	}
}
