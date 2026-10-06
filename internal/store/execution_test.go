package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
)

type executionWakeFixture struct {
	RunID      string `json:"runId"`
	Generation int64  `json:"generation"`
}

func (executionWakeFixture) Kind() string { return "store_execution_wake_fixture" }
func (executionWakeFixture) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

func executionAdmissionFixture(t *testing.T, s *Store) (protocol.Run, contract.Plan, EnqueueWake) {
	t.Helper()
	ctx := context.Background()
	legacy, plan := setupRun(t, s)
	var backend, definition string
	if err := s.Pool.QueryRow(ctx, "SELECT backend,definition_id FROM knotra_runs WHERE id=$1", legacy).Scan(&backend, &definition); err != nil {
		t.Fatal(err)
	}
	if backend != execution.BackendTemporal {
		t.Fatal("existing admissions did not remain Temporal")
	}
	plan.Digest = "frozen-plan"
	plan.Profile.Spec.Limits = contract.Limits{Timeout: "1h", MaxConcurrentNodes: 2, MaxNodeInstances: 100, MaxModelCalls: 10, MaxToolCalls: 10}
	m, err := rivermigrate.New(riverpgxv5.New(s.Pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{TargetVersion: 8}); err != nil {
		t.Fatal(err)
	}
	c, err := river.NewClient(riverpgxv5.New(s.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(ctx context.Context, tx pgx.Tx, id string, generation int64) error {
		_, err := c.InsertTx(ctx, tx, executionWakeFixture{RunID: id, Generation: generation}, nil)
		return err
	}
	return protocol.Run{ID: uuid.NewString(), DefinitionID: definition, Status: "pending", CreatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}, plan, enqueue
}

func TestRiverAdmissionRollsBackQueueFailureEvenIfCallerCommits(t *testing.T) {
	s := testStore(t)
	run, plan, enqueue := executionAdmissionFixture(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected enqueue failure")
	_, err = PutRiverRun(ctx, tx, run, plan, contract.Values{}, func(ctx context.Context, tx pgx.Tx, id string, generation int64) error {
		if err := enqueue(ctx, tx, id, generation); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("queue failure=%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var runs, jobs, scopes, timers int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_runs WHERE id=$1),
		(SELECT count(*) FROM river_job),(SELECT count(*) FROM knotra_execution_scopes),
		(SELECT count(*) FROM knotra_execution_timers)`, run.ID).Scan(&runs, &jobs, &scopes, &timers); err != nil {
		t.Fatal(err)
	}
	if runs+jobs+scopes+timers != 0 {
		t.Fatalf("partial admission runs=%d jobs=%d scopes=%d timers=%d", runs, jobs, scopes, timers)
	}
}

func TestRiverAdmissionProjectionAndGenerationTransactions(t *testing.T) {
	s := testStore(t)
	run, plan, enqueue := executionAdmissionFixture(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err = PutRiverRun(ctx, tx, run, plan, contract.Values{}, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if run.CreatedAt.Year() == 2000 {
		t.Fatal("application clock determined durable admission time")
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err := LockExecutionRun(ctx, tx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Backend != execution.BackendRiver || state.SchedulerVersion != execution.SchedulerVersion || state.StateFormatVersion != execution.StateFormatVersion || state.WakeGeneration != 1 || state.AppliedGeneration != 0 || !state.Deadline.Equal(state.AdmittedAt.Add(time.Hour)) {
		t.Fatalf("admission metadata=%+v", state)
	}
	if generation, err := WakeExecution(ctx, tx, run.ID, enqueue); err != nil || generation != 2 {
		t.Fatalf("wake=%d %v", generation, err)
	}
	if err := s.ProjectExecution(ctx, tx, execution.Projection{RunID: run.ID, Kind: "run", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var wake, jobs, events int
	var status string
	if err := s.Pool.QueryRow(ctx, `SELECT wake_generation,document->>'status',(SELECT count(*) FROM river_job),
		(SELECT count(*) FROM knotra_events WHERE run_id=$1) FROM knotra_runs WHERE id=$1`, run.ID).Scan(&wake, &status, &jobs, &events); err != nil {
		t.Fatal(err)
	}
	if wake != 1 || status != "pending" || jobs != 1 || events != 0 {
		t.Fatalf("rollback wake=%d status=%s jobs=%d events=%d", wake, status, jobs, events)
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WakeExecution(ctx, tx, run.ID, enqueue); err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectExecution(ctx, tx, execution.Projection{RunID: run.ID, Kind: "run", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyWake(ctx, tx, run.ID, 2); err != nil || !applied {
		t.Fatalf("apply=%v error=%v", applied, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := ApplyWake(ctx, tx, run.ID, 1); err != nil || applied {
		t.Fatalf("stale delivery mutated state: %v %v", applied, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = WakeExecution(ctx, tx, run.ID, func(ctx context.Context, tx pgx.Tx, id string, generation int64) error {
		if err := enqueue(ctx, tx, id, generation); err != nil {
			return err
		}
		return errors.New("enqueue failure")
	})
	if err == nil {
		t.Fatal("accepted failed wakeup")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var applied, outbox int
	if err := s.Pool.QueryRow(ctx, `SELECT wake_generation,applied_generation,(SELECT count(*) FROM river_job),
		(SELECT count(*) FROM knotra_events WHERE run_id=$1),(SELECT count(*) FROM knotra_outbox WHERE run_id=$1)
		FROM knotra_runs WHERE id=$1`, run.ID).Scan(&wake, &applied, &jobs, &events, &outbox); err != nil {
		t.Fatal(err)
	}
	if wake != 2 || applied != 2 || jobs != 2 || events != 1 || outbox != 0 {
		t.Fatalf("state wake=%d applied=%d jobs=%d events=%d legacyOutbox=%d", wake, applied, jobs, events, outbox)
	}
}

func TestExecutionClockIsCapturedAfterRunLockWait(t *testing.T) {
	s := testStore(t)
	run, plan, enqueue := executionAdmissionFixture(t, s)
	ctx := context.Background()
	admission, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err = PutRiverRun(ctx, admission, run, plan, contract.Values{}, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Rollback(ctx) }()
	if _, err := LockExecutionRun(ctx, owner, run.ID); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	if err := owner.QueryRow(ctx, "UPDATE knotra_runs SET execution_deadline=clock_timestamp()+interval '100 milliseconds' WHERE id=$1 RETURNING execution_deadline", run.ID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	result := make(chan execution.RunState, 1)
	errs := make(chan error, 1)
	go func() {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			errs <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		close(ready)
		state, err := LockExecutionRun(ctx, tx, run.ID)
		if err != nil {
			errs <- err
			return
		}
		result <- state
	}()
	<-ready
	time.Sleep(time.Until(deadline.Add(50 * time.Millisecond)))
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case state := <-result:
		if state.Deadline.After(state.Now) {
			t.Fatalf("lock wait extended deadline: deadline=%s now=%s", state.Deadline, state.Now)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run lock did not release")
	}
}

func TestUnsupportedExecutionVersionCannotWake(t *testing.T) {
	s := testStore(t)
	run, plan, enqueue := executionAdmissionFixture(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err = PutRiverRun(ctx, tx, run, plan, contract.Values{}, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE knotra_runs SET scheduler_version=scheduler_version+1 WHERE id=$1", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := WakeExecution(ctx, tx, run.ID, enqueue); !errors.Is(err, ErrExecutionVersion) {
		t.Fatalf("incompatible state woke: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var wake, jobs int
	if err := s.Pool.QueryRow(ctx, "SELECT wake_generation,(SELECT count(*) FROM river_job) FROM knotra_runs WHERE id=$1", run.ID).Scan(&wake, &jobs); err != nil {
		t.Fatal(err)
	}
	if wake != 1 || jobs != 1 {
		t.Fatalf("incompatible state mutated: wake=%d jobs=%d", wake, jobs)
	}
}
