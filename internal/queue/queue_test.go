package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

type testDatabase struct {
	pool   *pgxpool.Pool
	schema string
	config *pgxpool.Config
}

func newTestDatabase(t *testing.T, connections int32) testDatabase {
	t.Helper()
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for River PostgreSQL checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "knotra_qt_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = admin.Close(ctx)
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = connections
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return testDatabase{pool, schema, config}
}

func testHandlers() Handlers {
	return Handlers{
		Advance:  func(context.Context, *river.Job[AdvanceArgs]) error { return nil },
		Execute:  func(context.Context, *river.Job[ExecuteArgs]) error { return nil },
		Finalize: func(context.Context, *river.Job[FinalizeArgs]) error { return nil },
		Cleanup:  func(context.Context, *river.Job[CleanupArgs]) error { return nil },
	}
}

func newTestClient(t *testing.T, db testDatabase, engineID, hostID string, timeout time.Duration, h Handlers) *Client {
	t.Helper()
	c, err := New(db.pool, Config{EngineID: engineID, HostID: hostID, Schema: db.schema,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ExecutionWorkers: 2, ExecutionTimeout: timeout}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return c
}

func await(t *testing.T, timeout time.Duration, condition func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := condition()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for durable queue state")
}

func TestMigrationsResumeConcurrentlyWithSingleConnectionPools(t *testing.T) {
	db := newTestDatabase(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := rivermigrate.New(riverpgxv5.New(db.pool), &rivermigrate.Config{Schema: db.schema, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{TargetVersion: 4}); err != nil {
		t.Fatal(err)
	}
	other, err := pgxpool.NewWithConfig(ctx, db.config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	errorsSeen := make(chan error, 2)
	var wg sync.WaitGroup
	for _, pool := range []*pgxpool.Pool{db.pool, other} {
		wg.Add(1)
		go func() { defer wg.Done(); errorsSeen <- Migrate(ctx, pool, db.schema) }()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	versions, err := m.ExistingVersions(ctx)
	if err != nil || len(versions) != SchemaTarget {
		t.Fatalf("migration resume: %v, %v", versions, err)
	}
	if err := Migrate(ctx, db.pool, ""); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionalInsertionCompletionAndGenerationUniqueness(t *testing.T) {
	db := newTestDatabase(t, 8)
	ctx := context.Background()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, "CREATE TABLE spike_events(generation bigint PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	h := testHandlers()
	var completionRollbackChecked atomic.Bool
	h.Advance = func(ctx context.Context, job *river.Job[AdvanceArgs]) error {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "INSERT INTO spike_events VALUES($1)", job.Args.WakeGeneration); err != nil {
			return err
		}
		if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
			return err
		}
		if job.Args.WakeGeneration == 1 && !completionRollbackChecked.Load() {
			if err := tx.Rollback(ctx); err != nil {
				return err
			}
			var count int
			var state string
			if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM spike_events").Scan(&count); err != nil {
				return err
			}
			if err := db.pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id=$1", job.ID).Scan(&state); err != nil {
				return err
			}
			if count != 0 || state != "running" {
				return fmt.Errorf("completion rollback leaked: events=%d job=%s", count, state)
			}
			completionRollbackChecked.Store(true)
			tx, err = db.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, "INSERT INTO spike_events VALUES($1)", job.Args.WakeGeneration); err != nil {
				return err
			}
			if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	c := newTestClient(t, db, uuid.NewString(), "queue-test", time.Minute, h)
	args := AdvanceArgs{RunID: "run", WakeGeneration: 1, RoutingVersion: RoutingVersion}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertAdvance(ctx, tx, args); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO spike_events VALUES(99)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM river_job) + (SELECT count(*) FROM spike_events)").Scan(&count); err != nil || count != 0 {
		t.Fatalf("insertion rollback: %d, %v", count, err)
	}
	tx, err = db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.InsertAdvance(ctx, tx, args); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='completed'").Scan(&n)
		return n == 1, err
	})
	if !completionRollbackChecked.Load() {
		t.Fatal("did not exercise transactional completion rollback")
	}
	tx, err = db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertAdvance(ctx, tx, args); err != nil {
		t.Fatal(err)
	} // completed duplicate stays suppressed
	args.WakeGeneration++
	if err := c.InsertAdvance(ctx, tx, args); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='completed'").Scan(&n)
		return n == 2, err
	})
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM spike_events").Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate event: %d %v", count, err)
	}
}

func TestExecuteTimeoutDiscardsWithoutQueueRetry(t *testing.T) {
	db := newTestDatabase(t, 8)
	ctx := context.Background()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h := testHandlers()
	h.Execute = func(ctx context.Context, _ *river.Job[ExecuteArgs]) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	c := newTestClient(t, db, uuid.NewString(), "queue-test", 100*time.Millisecond, h)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertExecute(ctx, tx, ExecuteArgs{Attempt: execution.AttemptID{RunID: "run", InstanceID: "a", Number: 1}, DispatchGeneration: 1, RoutingVersion: RoutingVersion}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='discarded' AND attempt=1 AND max_attempts=1").Scan(&n)
		return n == 1, err
	})
	if calls.Load() != 1 {
		t.Fatalf("queue repeated physical execution %d times", calls.Load())
	}
}

func TestDiscardedExecutionFinalizesOnceAcrossTwoClients(t *testing.T) {
	db := newTestDatabase(t, 12)
	ctx := context.Background()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `CREATE TABLE spike_attempt(state text NOT NULL, outcome_key text NOT NULL, operation_complete bool NOT NULL DEFAULT false, result jsonb);
		CREATE TABLE spike_events(id integer PRIMARY KEY);
		INSERT INTO spike_attempt(state,outcome_key) VALUES('ready','')`); err != nil {
		t.Fatal(err)
	}
	files, err := execution.OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	owner := execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "a", Number: 1}, WorkerID: "incarnation", Generation: 1}
	key, _ := owner.OutcomeKey()
	var calls atomic.Int32
	h := testHandlers()
	h.Execute = func(ctx context.Context, _ *river.Job[ExecuteArgs]) error {
		claim, err := db.pool.Exec(ctx, "UPDATE spike_attempt SET state='claimed',outcome_key=$1 WHERE state='ready'", key)
		if err != nil {
			return err
		}
		if claim.RowsAffected() == 0 {
			return nil
		}
		calls.Add(1) // fake external operation, outside claim transaction
		if _, err := db.pool.Exec(ctx, "UPDATE spike_attempt SET operation_complete=true WHERE state='claimed'"); err != nil {
			return err
		}
		_, err = files.Put(execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: owner, PlanID: "plan", CompletedAt: time.Now().UTC(), Outputs: contract.Values{"result": {JSON: []byte(`"confirmed"`)}}})
		if err != nil {
			return err
		}
		return errors.New("injected loss of publication after durable handoff")
	}
	h.Finalize = func(ctx context.Context, job *river.Job[FinalizeArgs]) error {
		outcome, err := files.Get(job.Args.OutcomeKey)
		if err != nil {
			return err
		}
		if outcome.Ownership.AttemptID != job.Args.Attempt {
			return errors.New("wrong attempt evidence")
		}
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		updated, err := tx.Exec(ctx, "UPDATE spike_attempt SET state='completed',result=$2 WHERE state='claimed' AND operation_complete AND outcome_key=$1", job.Args.OutcomeKey, outcome.Outputs["result"].JSON)
		if err != nil {
			return err
		}
		if updated.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, "INSERT INTO spike_events VALUES(1)"); err != nil {
				return err
			}
			client := river.ClientFromContext[pgx.Tx](ctx)
			if _, err := client.InsertTx(ctx, tx, AdvanceArgs{RunID: "run", WakeGeneration: 2, RoutingVersion: RoutingVersion}, &river.InsertOpts{Queue: "knotra_advance_" + engineIDFromQueue(job.Queue)}); err != nil {
				return err
			}
		}
		if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	engineID := uuid.NewString()
	c1 := newTestClient(t, db, engineID, "queue-test", time.Minute, h)
	c2 := newTestClient(t, db, engineID, "queue-test", time.Minute, h)
	if err := c1.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	args := ExecuteArgs{Attempt: owner.AttemptID, DispatchGeneration: 1, RoutingVersion: RoutingVersion}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.InsertExecute(ctx, tx, args); err != nil {
		t.Fatal(err)
	}
	// A second queue delivery bypasses queue uniqueness but cannot bypass domain ownership.
	args.DispatchGeneration++
	if err := c2.InsertExecute(ctx, tx, args); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='discarded'").Scan(&n)
		return n == 1, err
	})
	for _, c := range []*Client{c1, c2} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: owner.AttemptID, OutcomeKey: key, RoutingVersion: RoutingVersion}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM spike_attempt WHERE state='completed' AND result='\"confirmed\"'::jsonb").Scan(&n)
		return n == 1, err
	})
	if calls.Load() != 1 {
		t.Fatalf("external operation count=%d", calls.Load())
	}
	var events int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM spike_events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("publication count=%d error=%v", events, err)
	}
	var advances int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind=$1", (AdvanceArgs{}).Kind()).Scan(&advances); err != nil || advances != 1 {
		t.Fatalf("wake publication=%d %v", advances, err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind=$1 AND state='completed'", (FinalizeArgs{}).Kind()).Scan(&n)
		return n == 1, err
	})
	// Queue retention removes uniqueness evidence, never domain execution state.
	if _, err := db.pool.Exec(ctx, "DELETE FROM river_job WHERE kind=$1 AND state='completed'", (FinalizeArgs{}).Kind()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: owner.AttemptID, OutcomeKey: key, RoutingVersion: RoutingVersion}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 10*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind=$1 AND state='completed'", (FinalizeArgs{}).Kind()).Scan(&n)
		return n == 1, err
	})
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM spike_events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("retention replay published duplicate evidence: %d %v", events, err)
	}
}

func engineIDFromQueue(queue string) string { return strings.TrimPrefix(queue, "knotra_maintenance_") }

func TestInsertOptionsKeepBusinessAndInfrastructureRetriesSeparate(t *testing.T) {
	if (ExecuteArgs{}).InsertOpts().MaxAttempts != 1 {
		t.Fatal("execution can be implicitly retried")
	}
	for _, opts := range []river.InsertOpts{(AdvanceArgs{}).InsertOpts(), (ExecuteArgs{}).InsertOpts(), (FinalizeArgs{}).InsertOpts()} {
		if !opts.UniqueOpts.ByArgs || !opts.UniqueOpts.ByQueue {
			t.Fatal("generation uniqueness lost")
		}
		found := false
		for _, state := range opts.UniqueOpts.ByState {
			found = found || state == rivertype.JobStateCompleted
		}
		if !found {
			t.Fatal("completed generation no longer deduplicates")
		}
	}
}

func TestCleanupDeliveryIsIsolatedByEngineAndHost(t *testing.T) {
	db := newTestDatabase(t, 8)
	ctx := t.Context()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	engineID := uuid.NewString()
	var deliveries [2]atomic.Int32
	clients := make([]*Client, 2)
	for i, host := range []string{"host-one", "host-two"} {
		handlers := testHandlers()
		handlers.Cleanup = func(_ context.Context, job *river.Job[CleanupArgs]) error {
			if job.Args.HostID != host {
				t.Errorf("host %s fetched cleanup for %s", host, job.Args.HostID)
			}
			deliveries[i].Add(1)
			return nil
		}
		clients[i] = newTestClient(t, db, engineID, host, time.Minute, handlers)
		if err := clients[i].Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if clients[0].cleanup == clients[1].cleanup || len(clients[0].cleanup) > 64 {
		t.Fatal("cleanup queue lost host isolation or exceeds River's name limit")
	}
	for _, client := range clients {
		if err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
			return client.InsertCleanup(ctx, tx, CleanupArgs{HostID: client.hostID, WorkerID: "original-owner", ResourceID: uuid.NewString(), OwnershipGeneration: 1, DispatchGeneration: 1, RoutingVersion: RoutingVersion})
		}); err != nil {
			t.Fatal(err)
		}
	}
	await(t, 5*time.Second, func() (bool, error) {
		return deliveries[0].Load() == 1 && deliveries[1].Load() == 1, nil
	})
	if err := pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		return clients[0].InsertCleanup(ctx, tx, CleanupArgs{HostID: "host-two", WorkerID: "original-owner", ResourceID: uuid.NewString(), OwnershipGeneration: 1, DispatchGeneration: 1, RoutingVersion: RoutingVersion})
	}); err == nil {
		t.Fatal("client accepted cleanup for another host")
	}
}

func TestQueueRunsWithSingleConnectionPool(t *testing.T) {
	db := newTestDatabase(t, 1)
	ctx := context.Background()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	var latencyMu sync.Mutex
	var latencies []time.Duration
	h := testHandlers()
	h.Advance = func(ctx context.Context, job *river.Job[AdvanceArgs]) error {
		var now time.Time
		if err := db.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return err
		}
		starts.Add(1)
		latencyMu.Lock()
		latencies = append(latencies, now.Sub(job.CreatedAt))
		latencyMu.Unlock()
		return nil
	}
	c := newTestClient(t, db, uuid.NewString(), "queue-test", time.Minute, h)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := c.InsertAdvance(ctx, tx, AdvanceArgs{RunID: "run", WakeGeneration: int64(i + 1), RoutingVersion: RoutingVersion}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 15*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='completed'").Scan(&n)
		return n == 20, err
	})
	if starts.Load() != 20 {
		t.Fatalf("deliveries=%d", starts.Load())
	}
	t.Logf("20 queued jobs fetched, queried and completed in %s with MaxConns=1", time.Since(start))
	latencyMu.Lock()
	defer latencyMu.Unlock()
	var total, maximum time.Duration
	for _, latency := range latencies {
		total += latency
		if latency > maximum {
			maximum = latency
		}
	}
	t.Logf("database creation to handler query latency: mean=%s maximum=%s", total/time.Duration(len(latencies)), maximum)
}

func TestFetchedUnsupportedVersionCannotInvokeHandler(t *testing.T) {
	var calls int
	w := deliveryWorker[ExecuteArgs]{work: func(context.Context, *river.Job[ExecuteArgs]) error { calls++; return nil }}
	err := w.Work(context.Background(), &river.Job[ExecuteArgs]{Args: ExecuteArgs{
		Attempt: execution.AttemptID{RunID: "run", InstanceID: "a", Number: 1}, DispatchGeneration: 1, RoutingVersion: RoutingVersion + 1,
	}})
	if err == nil || calls != 0 {
		t.Fatalf("incompatible delivery invoked executor: calls=%d err=%v", calls, err)
	}
}
