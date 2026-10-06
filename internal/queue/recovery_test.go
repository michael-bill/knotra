package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

// outageProxy isolates a real PostgreSQL connectivity outage to the child
// worker, leaving the test's administrative connection and other schemas alone.
type outageProxy struct {
	ctx         context.Context
	listener    net.Listener
	target      string
	mu          sync.Mutex
	online      bool
	connections map[net.Conn]bool
	done        chan struct{}
}

func newOutageProxy(t *testing.T, target string) *outageProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &outageProxy{ctx: t.Context(), listener: ln, target: target, online: true, connections: map[net.Conn]bool{}, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			incoming, err := ln.Accept()
			if err != nil {
				return
			}
			go p.forward(incoming)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-p.done; p.setOnline(false) })
	return p
}

func (p *outageProxy) forward(incoming net.Conn) {
	defer func() { _ = incoming.Close() }()
	p.mu.Lock()
	if !p.online {
		p.mu.Unlock()
		return
	}
	p.connections[incoming] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, incoming); p.mu.Unlock() }()
	remote, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(p.ctx, "tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = remote.Close() }()
	p.mu.Lock()
	if !p.online {
		p.mu.Unlock()
		return
	}
	p.connections[remote] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, remote); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(remote, incoming); _ = remote.Close(); close(done) }()
	_, _ = io.Copy(incoming, remote)
	_ = incoming.Close()
	<-done
}

func (p *outageProxy) setOnline(online bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.online = online
	if !online {
		for conn := range p.connections {
			_ = conn.Close()
		}
	}
}

func writeSignal(directory, name string) error {
	return os.WriteFile(filepath.Join(directory, name), []byte("ready"), 0600)
}

func waitSignal(ctx context.Context, directory, name string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runRecoveryChild(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	directory := os.Getenv("KNOTRA_RIVER_SPIKE_DIRECTORY")
	dsn := os.Getenv("KNOTRA_RIVER_SPIKE_DSN")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	owner := execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "a", Number: 1}, WorkerID: "crashed-worker", Generation: 1}
	key, _ := owner.OutcomeKey()
	h := testHandlers()
	h.Execute = func(ctx context.Context, _ *river.Job[ExecuteArgs]) error {
		claim, err := pool.Exec(ctx, "UPDATE spike_attempt SET state='claimed',outcome_key=$1 WHERE state='ready'", key)
		if err != nil {
			return err
		}
		if claim.RowsAffected() == 0 {
			return nil
		}
		// This fsynced append is a fake external service's durable operation log.
		log, err := os.OpenFile(filepath.Join(directory, "external-calls"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		if _, err := log.WriteString("external operation\n"); err != nil {
			_ = log.Close()
			return err
		}
		if err := log.Sync(); err != nil {
			_ = log.Close()
			return err
		}
		if err := log.Close(); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, "UPDATE spike_attempt SET operation_complete=true WHERE state='claimed'"); err != nil {
			return err
		}
		if err := writeSignal(directory, "journaled"); err != nil {
			return err
		}
		if err := waitSignal(ctx, directory, "outage-started"); err != nil {
			return err
		}
		// Complete evidence is handed off while PostgreSQL is unreachable.
		if _, err := files.Put(execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: owner, PlanID: "plan", CompletedAt: time.Now().UTC(), Outputs: contract.Values{"result": {JSON: []byte(`"confirmed"`)}}}); err != nil {
			return err
		}
		if err := writeSignal(directory, "outcome-stored"); err != nil {
			return err
		}
		publicationCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := pool.Ping(publicationCtx); err == nil {
			return errors.New("outage did not disconnect publication")
		}
		if err := writeSignal(directory, "publication-unavailable"); err != nil {
			return err
		}
		<-ctx.Done() // parent sends SIGKILL, bypassing worker shutdown
		return ctx.Err()
	}
	h.Finalize = func(ctx context.Context, job *river.Job[FinalizeArgs]) error {
		var discarded bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM river_job WHERE kind=$1 AND state='discarded')", (ExecuteArgs{}).Kind()).Scan(&discarded); err != nil {
			return err
		}
		if !discarded {
			return river.JobSnooze(100 * time.Millisecond)
		}
		outcome, err := files.Get(job.Args.OutcomeKey)
		if err != nil {
			return err
		}
		if outcome.Ownership.AttemptID != job.Args.Attempt || outcome.PlanID != "plan" {
			return errors.New("mismatched recovery evidence")
		}
		result, err := json.Marshal(outcome.Outputs)
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		updated, err := tx.Exec(ctx, "UPDATE spike_attempt SET state='completed',result=$2 WHERE state='claimed' AND operation_complete AND outcome_key=$1", job.Args.OutcomeKey, result)
		if err != nil {
			return err
		}
		if updated.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, "INSERT INTO spike_events VALUES(1)"); err != nil {
				return err
			}
			client := river.ClientFromContext[pgx.Tx](ctx)
			if _, err := client.InsertTx(ctx, tx, AdvanceArgs{RunID: "run", WakeGeneration: 2, RoutingVersion: RoutingVersion}, &river.InsertOpts{Queue: "knotra_advance_" + os.Getenv("KNOTRA_RIVER_SPIKE_ENGINE")}); err != nil {
				return err
			}
		}
		if _, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return writeSignal(directory, "finalized")
	}
	c := newTestClient(t, testDatabase{pool: pool, schema: os.Getenv("KNOTRA_RIVER_SPIKE_SCHEMA")}, os.Getenv("KNOTRA_RIVER_SPIKE_ENGINE"), "queue-test", 30*time.Second, h)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KNOTRA_RIVER_SPIKE_MODE") == "finalize" {
		if err := waitSignal(ctx, directory, "finalized"); err != nil {
			t.Fatal(err)
		}
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := c.Stop(stopCtx); err != nil {
			t.Fatal(err)
		}
		return
	}
	<-ctx.Done()
	t.Fatal("child was not terminated at the crash point")
}

func TestSixMinuteDatabaseOutageAndProcessTermination(t *testing.T) {
	if os.Getenv("KNOTRA_RIVER_SPIKE_CHILD") == "1" {
		runRecoveryChild(t)
		return
	}
	if os.Getenv("KNOTRA_TEST_RIVER_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RIVER_RECOVERY=1 for six-minute outage and process crash")
	}
	db := newTestDatabase(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `CREATE TABLE spike_attempt(state text NOT NULL,outcome_key text NOT NULL,operation_complete bool NOT NULL DEFAULT false,result jsonb);
		CREATE TABLE spike_events(id integer PRIMARY KEY); INSERT INTO spike_attempt(state,outcome_key) VALUES('ready','')`); err != nil {
		t.Fatal(err)
	}
	config := db.config.ConnConfig.Copy()
	proxy := newOutageProxy(t, net.JoinHostPort(config.Host, fmt.Sprint(config.Port)))
	childURL, err := url.Parse(os.Getenv("KNOTRA_TEST_DATABASE_URL"))
	if err != nil || (childURL.Scheme != "postgres" && childURL.Scheme != "postgresql") {
		t.Fatal("process recovery test requires a postgres:// database URL")
	}
	childURL.Host = proxy.listener.Addr().String()
	params := childURL.Query()
	params.Set("search_path", db.schema)
	childURL.RawQuery = params.Encode()
	engineID := uuid.NewString()
	c := newTestClient(t, db, engineID, "queue-test", time.Minute, testHandlers()) // insertion only; the child executes
	owner := execution.Ownership{AttemptID: execution.AttemptID{RunID: "run", InstanceID: "a", Number: 1}, WorkerID: "crashed-worker", Generation: 1}
	key, _ := owner.OutcomeKey()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertExecute(ctx, tx, ExecuteArgs{Attempt: owner.AttemptID, DispatchGeneration: 1, RoutingVersion: RoutingVersion}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	startChild := func(mode string) *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSixMinuteDatabaseOutageAndProcessTermination$")
		cmd.Env = append(os.Environ(), "KNOTRA_RIVER_SPIKE_CHILD=1", "KNOTRA_RIVER_SPIKE_MODE="+mode, "KNOTRA_RIVER_SPIKE_DIRECTORY="+directory, "KNOTRA_RIVER_SPIKE_DSN="+childURL.String(), "KNOTRA_RIVER_SPIKE_SCHEMA="+db.schema, "KNOTRA_RIVER_SPIKE_ENGINE="+engineID)
		output, err := os.Create(filepath.Join(directory, mode+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Start(); err != nil {
			_ = output.Close()
			t.Fatal(err)
		}
		_ = output.Close()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	child := startChild("execute")
	if err := waitSignal(ctx, directory, "journaled"); err != nil {
		t.Fatal(err)
	}
	outageStart := time.Now()
	proxy.setOnline(false)
	if err := writeSignal(directory, "outage-started"); err != nil {
		t.Fatal(err)
	}
	if err := waitSignal(ctx, directory, "publication-unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("crash child exited normally")
	}
	t.Log("outcome stored during real PostgreSQL connection outage; worker terminated with SIGKILL")
	// Keep connectivity unavailable for six full minutes after the initial cut.
	timer := time.NewTimer(time.Until(outageStart.Add(6 * time.Minute)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	proxy.setOnline(true)
	// Age the abandoned delivery past the configured rescue horizon. The new
	// process must discard its exhausted delivery and finalize domain evidence.
	if _, err := db.pool.Exec(ctx, "UPDATE river_job SET attempted_at=clock_timestamp()-interval '3 hours' WHERE kind=$1 AND state='running'", (ExecuteArgs{}).Kind()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: owner.AttemptID, OutcomeKey: key, RoutingVersion: RoutingVersion}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	replacement := startChild("finalize")
	if err := replacement.Wait(); err != nil {
		data, _ := os.ReadFile(filepath.Join(directory, "finalize.log"))
		t.Fatalf("replacement failed: %v\n%s", err, data)
	}
	var completed, events, wakeups int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM spike_attempt WHERE state='completed'),(SELECT count(*) FROM spike_events),(SELECT count(*) FROM river_job WHERE kind='knotra_advance_v1')`).Scan(&completed, &events, &wakeups); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || events != 1 || wakeups != 1 {
		t.Fatalf("recovery publication=%d events=%d wakeups=%d", completed, events, wakeups)
	}
	log, err := os.ReadFile(filepath.Join(directory, "external-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "external operation\n") != 1 {
		t.Fatalf("external operation replayed: %q", log)
	}
	t.Logf("recovered after %s with one external operation and one publication", time.Since(outageStart))
}

func TestLongExecutionExceedsRiverDefaultTimeout(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RIVER_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RIVER_RECOVERY=1 for long-running queue check")
	}
	db := newTestDatabase(t, 8)
	ctx := context.Background()
	if err := Migrate(ctx, db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h := testHandlers()
	h.Execute = func(ctx context.Context, _ *river.Job[ExecuteArgs]) error {
		calls.Add(1)
		timer := time.NewTimer(65 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	c := newTestClient(t, db, uuid.NewString(), "queue-test", 2*time.Minute, h)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.InsertExecute(ctx, tx, ExecuteArgs{Attempt: execution.AttemptID{RunID: "run", InstanceID: "long", Number: 1}, DispatchGeneration: 1, RoutingVersion: RoutingVersion}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, 80*time.Second, func() (bool, error) {
		var n int
		err := db.pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state='completed' AND attempt=1").Scan(&n)
		return n == 1, err
	})
	if calls.Load() != 1 {
		t.Fatalf("long execution replayed %d times", calls.Load())
	}
}
