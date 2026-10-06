//go:build unix

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/client"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/queue"
	"github.com/michael-bill/knotra/internal/store"
	storedb "github.com/michael-bill/knotra/internal/store/db"
)

func TestRiverStartupProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for startup SIGKILL checks")
	}
	for _, mode := range []string{"migration_before_commit", "registration_before_commit", "registered_before_binding"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			dsn, err := url.Parse(databaseSchema(t, ctx))
			if err != nil {
				t.Fatal(err)
			}
			parameters := dsn.Query()
			parameters.Set("pool_max_conns", "1")
			dsn.RawQuery = parameters.Encode()
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: dsn.String(), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: routingHumanPipeline, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(routingHumanPipeline)}}}
			plan, diagnostics := contract.Compile(pkg, nil)
			if contract.HasErrors(diagnostics) {
				t.Fatal(diagnostics)
			}
			definition := protocol.Definition{ID: uuid.NewString(), Name: "routing-human", Title: "routing-human", PackageDigest: plan.Digest, CreatedAt: time.Now().UTC(), Package: pkg}
			if err := pgx.BeginFunc(ctx, admin.Pool, func(tx pgx.Tx) error {
				var err error
				definition, err = store.PutDefinition(ctx, tx, definition)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			schema, err := storedb.New(admin.Pool).CurrentSchema(ctx)
			if err != nil {
				t.Fatal(err)
			}
			barrier, err := pgx.ConnectConfig(ctx, admin.Pool.Config().ConnConfig.Copy())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Close(context.Background()) }()
			var transaction pgx.Tx
			if mode == "migration_before_commit" {
				migrator, err := rivermigrate.New(riverpgxv5.New(admin.Pool), &rivermigrate.Config{Schema: schema})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{TargetVersion: 4}); err != nil {
					t.Fatal(err)
				}
				transaction, err = barrier.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = transaction.Rollback(context.Background()) }()
				if _, err := transaction.Exec(ctx, "LOCK TABLE river_queue IN ACCESS SHARE MODE"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := queue.Migrate(ctx, admin.Pool, schema); err != nil {
					t.Fatal(err)
				}
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273657)"); err != nil {
					t.Fatal(err)
				}
				event, condition := "INSERT", "true"
				if mode == "registered_before_binding" {
					event, condition = "UPDATE", "OLD.delivery_client_id IS NULL AND NEW.delivery_client_id IS NOT NULL"
				}
				if _, err := admin.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION startup_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273657); RETURN NEW; END $$;
     CREATE TRIGGER startup_crash_barrier BEFORE %s ON knotra_workers FOR EACH ROW WHEN (%s) EXECUTE FUNCTION startup_crash_barrier()`, event, condition)); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(work, "before.log")
			first := startEngineProcess(t, ctx, optionsPath, logPath, nil)
			awaitLifecycleBoundary(t, ctx, first, logPath, func() bool {
				var blocked bool
				if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrier.PgConn().PID())).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				return blocked
			})
			assertLifecycleListenerClosed(t, ctx, options.Listen)
			var workers, bound, version int
			if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_workers),
                (SELECT count(*) FROM knotra_workers WHERE delivery_client_id IS NOT NULL),
                (SELECT max(version) FROM river_migration)`).Scan(&workers, &bound, &version); err != nil {
				t.Fatal(err)
			}
			wantWorkers, wantVersion := 0, queue.SchemaTarget
			if mode == "registered_before_binding" {
				wantWorkers = 1
			}
			if mode == "migration_before_commit" {
				wantVersion = 6
			}
			if workers != wantWorkers || bound != 0 || version != wantVersion {
				t.Fatalf("startup barrier workers/bound/version=%d/%d/%d want=%d/0/%d", workers, bound, version, wantWorkers, wantVersion)
			}
			first.kill(t)
			assertLifecycleSIGKILL(t, first)
			if transaction != nil {
				if err := transaction.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273657)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER startup_crash_barrier ON knotra_workers"); err != nil {
					t.Fatal(err)
				}
			}
			// A blocked PostgreSQL backend may notice socket loss only after its blocker
			// releases. Reacquiring the real service lease also waits for that teardown.
			var lease *store.Lease
			awaitLifecycleBoundary(t, ctx, nil, "", func() bool {
				lease, err = admin.AcquireLease(ctx)
				return err == nil
			})
			if err := lease.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_workers),
    (SELECT count(*) FROM knotra_workers WHERE delivery_client_id IS NOT NULL),
    (SELECT max(version) FROM river_migration)`).Scan(&workers, &bound, &version); err != nil {
				t.Fatal(err)
			}
			// Registration and binding are autocommit statements: after the
			// socket disappears, PostgreSQL may still commit the blocked one.
			// Either outcome leaves only an unused incarnation, never work.
			if version != wantVersion || workers < wantWorkers || workers > 1 || bound > workers ||
				(mode != "registered_before_binding" && bound != 0) ||
				(mode == "migration_before_commit" && workers != 0) {
				t.Fatalf("interrupted startup workers/bound/version=%d/%d/%d", workers, bound, version)
			}
			retainedWorkers, retainedBound := workers, bound

			if mode == "migration_before_commit" {
				var rolledBack bool
				if err := admin.Pool.QueryRow(ctx, `SELECT to_regclass('river_notification') IS NULL AND to_regclass('river_client') IS NOT NULL AND to_regclass('river_client_queue') IS NOT NULL`).Scan(&rolledBack); err != nil || !rolledBack {
					t.Fatalf("migration 7 did not roll back atomically: %t %v", rolledBack, err)
				}
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != admin.EngineID {
				t.Fatal("replacement changed the engine identity")
			}
			var saved struct{ Definition protocol.Definition }
			if err := api.Get(ctx, "/definitions/"+definition.ID, &saved); err != nil || saved.Definition.PackageDigest != definition.PackageDigest {
				t.Fatalf("startup lost the retained definition: %+v %v", saved, err)
			}
			var accepted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			request := awaitHuman(t, ctx, api, accepted.Run.ID)
			if err := api.Command(ctx, "/requests/"+request.ID+"/response", map[string]any{"outputs": map[string]any{"approved": true}}, uuid.NewString(), new(any)); err != nil {
				t.Fatal(err)
			}
			final := awaitTerminal(t, ctx, api, accepted.Run.ID)
			if final.Status != "succeeded" || string(final.Outputs["approved"]) != "true" {
				t.Fatalf("replacement failed retained definition: %+v", final)
			}
			var slots int
			var migrated bool
			if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_workers),
    (SELECT count(*) FROM knotra_workers WHERE delivery_client_id IS NOT NULL),
    (SELECT max(version) FROM river_migration),COALESCE((SELECT sum(active_attempts) FROM knotra_execution_scopes),0),
    to_regclass('river_notification') IS NOT NULL AND to_regclass('river_client') IS NULL AND to_regclass('river_client_queue') IS NULL`).Scan(&workers, &bound, &version, &slots, &migrated); err != nil {
				t.Fatal(err)
			}
			if workers != retainedWorkers+1 || bound != retainedBound+1 || version != queue.SchemaTarget || slots != 0 || !migrated {
				t.Fatalf("replacement workers/bound/version/slots/migrated=%d/%d/%d/%d/%t", workers, bound, version, slots, migrated)
			}
		})
	}
}

func TestRiverShutdownProcessBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_RECOVERY and KNOTRA_TEST_DATABASE_URL for service shutdown checks")
	}
	t.Setenv("KNOTRA_LIFECYCLE_FIXTURE_KEY", "fixture-key")
	for _, mode := range []string{"drain_before_commit", "draining_before_response", "graceful_active_response", "lease_lost_active_response", "forced_active_timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			graceful := mode == "graceful_active_response" || mode == "lease_lost_active_response"
			var calls atomic.Int32
			var interrupted atomic.Bool
			entered := make(chan struct{}, 1)
			respond, release := context.WithCancel(ctx)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				calls.Add(1)
				body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil || !bytes.Contains(body, []byte("Return answer 42.")) || request.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("physical request lost frozen prompt or credentials: bytes=%d error=%v", len(body), err)
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-respond.Done():
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"knotra_output","arguments":"{\"answer\":42}"}]}`)
				case <-request.Context().Done():
					interrupted.Store(true)
				}
			}))
			defer func() { release(); provider.Close() }()
			work := t.TempDir()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			profile.Spec.Limits.Timeout = "3m"
			profile.Spec.Secrets = map[string]contract.SecretSource{"fixture": {Env: "KNOTRA_LIFECYCLE_FIXTURE_KEY"}}
			profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {SecretRef: "fixture"}}}}
			profilePath := filepath.Join(work, "profile.json")
			writeJSON(t, profilePath, profile)
			options := app.Options{Backend: execution.BackendRiver, Listen: unusedAddress(t), DatabaseURL: databaseSchema(t, ctx), TemporalAddress: "127.0.0.1:1", DataDir: filepath.Join(work, "engine"), Profiles: []string{profilePath}}
			optionsPath := filepath.Join(work, "server.json")
			writeJSON(t, optionsPath, options)
			api := &client.Client{BaseURL: "http://" + options.Listen, StateDir: filepath.Join(work, "client")}
			logPath := filepath.Join(work, "before.log")
			first := startEngineProcess(t, ctx, optionsPath, logPath, api)
			t.Cleanup(func() {
				if t.Failed() {
					log, _ := os.ReadFile(logPath)
					t.Logf("service shutdown log: %s", log)
				}
			})
			var definition struct{ Definition protocol.Definition }
			source := routingModelPipeline
			if mode == "forced_active_timeout" {
				source = strings.Replace(source, "      type: llm\n", "      type: llm\n      execution: {retry: {maxAttempts: 2, backoff: 1ms}}\n", 1)
			}
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: source, Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(source)}}}
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var accepted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("physical model request never started", ctx.Err())
			}
			admin, err := store.Open(ctx, options.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var workerID string
			var rootDeadline, nodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT a.owner,r.execution_deadline,n.deadline
    FROM knotra_execution_attempts a JOIN knotra_runs r ON r.id=a.run_id
    JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id
    WHERE a.run_id=$1 AND a.state='claimed'`, accepted.Run.ID).Scan(&workerID, &rootDeadline, &nodeDeadline); err != nil {
				t.Fatal(err)
			}
			var barrier *pgx.Conn
			if mode == "drain_before_commit" {
				barrier, err = pgx.ConnectConfig(ctx, admin.Pool.Config().ConnConfig.Copy())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273658)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, `CREATE FUNCTION shutdown_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273658); RETURN NEW; END $$;
     CREATE TRIGGER shutdown_crash_barrier BEFORE UPDATE ON knotra_workers FOR EACH ROW WHEN (NOT OLD.draining AND NEW.draining) EXECUTE FUNCTION shutdown_crash_barrier()`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "lease_lost_active_response" {
				var terminated bool
				if err := admin.Pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
                    WHERE locktype='advisory' AND granted AND objsubid=1
                    AND classid=((hashtextextended($1,0)>>32)&4294967295)::oid
                    AND objid=(hashtextextended($1,0)&4294967295)::oid`, "knotra.engine."+admin.EngineID).Scan(&terminated); err != nil || !terminated {
					t.Fatalf("did not terminate the service ownership connection: %t %v", terminated, err)
				}
			} else if err := first.command.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			awaitLifecycleBoundary(t, ctx, first, logPath, func() bool {
				var reached bool
				if barrier != nil {
					if err := admin.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, int64(barrier.PgConn().PID())).Scan(&reached); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := admin.Pool.QueryRow(ctx, "SELECT draining FROM knotra_workers WHERE id=$1", workerID).Scan(&reached); err != nil {
						t.Fatal(err)
					}
				}
				return reached
			})
			assertLifecycleListenerClosed(t, ctx, options.Listen)
			var draining bool
			var debits, operations, completed, slots int
			if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT draining FROM knotra_workers WHERE id=$2),
    (SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
    (SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND admitted_at IS NOT NULL),
    (SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND completed),
    (SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1)`, accepted.Run.ID, workerID).Scan(&draining, &debits, &operations, &completed, &slots); err != nil {
				t.Fatal(err)
			}
			if draining != (barrier == nil) || debits != 1 || operations != 1 || completed != 0 || slots != 1 || calls.Load() != 1 {
				t.Fatalf("shutdown barrier draining/debits/operations/completed/slots/calls=%t/%d/%d/%d/%d/%d", draining, debits, operations, completed, slots, calls.Load())
			}
			switch {
			case graceful:
				release()
				select {
				case <-first.finished:
				case <-ctx.Done():
					t.Fatal("graceful shutdown did not exit", ctx.Err())
				}
				log, _ := os.ReadFile(logPath)
				if mode == "lease_lost_active_response" {
					if first.command.ProcessState.Success() || !bytes.Contains(log, []byte("engine ownership connection lost; stopping service")) {
						t.Fatalf("ownership loss did not fail and stop the service: %s", log)
					}
				} else if !first.command.ProcessState.Success() {
					t.Fatalf("graceful shutdown failed: %s", log)
				}
				if err := admin.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND completed", accepted.Run.ID).Scan(&completed); err != nil || completed != 1 {
					t.Fatalf("graceful shutdown lost the active response: %d %v", completed, err)
				}
			case mode == "forced_active_timeout":
				stopped, stop := context.WithTimeout(ctx, 25*time.Second)
				defer stop()
				select {
				case <-first.finished:
				case <-stopped.Done():
					t.Fatal("bounded shutdown did not cancel the active request and exit", stopped.Err())
				}
				log, _ := os.ReadFile(logPath)
				if first.command.ProcessState.Success() || !bytes.Contains(log, []byte("execution shutdown failed")) || !bytes.Contains(log, []byte("context deadline exceeded")) {
					t.Fatalf("forced shutdown did not report its drain timeout: %s", log)
				}
				awaitLifecycleBoundary(t, stopped, nil, "", interrupted.Load)
			default:
				first.kill(t)
				assertLifecycleSIGKILL(t, first)
			}
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273658)"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.Pool.Exec(ctx, "DROP TRIGGER shutdown_crash_barrier ON knotra_workers"); err != nil {
					t.Fatal(err)
				}
			}
			var lease *store.Lease
			awaitLifecycleBoundary(t, ctx, nil, "", func() bool { lease, err = admin.AcquireLease(ctx); return err == nil })
			if err := lease.Close(ctx); err != nil {
				t.Fatal(err)
			}
			startEngineProcess(t, ctx, optionsPath, filepath.Join(work, "after.log"), api)
			if engineIdentity(t, ctx, api) != admin.EngineID {
				t.Fatal("shutdown replacement changed engine identity")
			}
			if !graceful {
				awaitLifecycleBoundary(t, ctx, nil, "", func() bool {
					status, err := store.ReadRunStatus(ctx, admin.Pool, accepted.Run.ID)
					if err != nil || protocol.Terminal(status) {
						t.Fatalf("lost model call terminated before resolution: %s %v", status, err)
					}
					return status == "waiting_resolution"
				})
				var requestID string
				if err := admin.Pool.QueryRow(ctx, "SELECT id FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open'", accepted.Run.ID).Scan(&requestID); err != nil {
					t.Fatal(err)
				}
				resolution, err := admin.Request(ctx, requestID)
				if err != nil || resolution.Failure == nil || !resolution.Failure.Unknown || calls.Load() != 1 {
					t.Fatalf("shutdown loss repeated the provider or lost uncertainty: %+v calls=%d error=%v", resolution, calls.Load(), err)
				}
				// The fixture's one physical invocation is known to have returned no reply.
				// Cancel after inspecting the uncertainty; do not invent a successful result.
				if err := api.Command(ctx, "/runs/"+accepted.Run.ID+"/cancel", map[string]any{}, uuid.NewString(), new(any)); err != nil {
					t.Fatal(err)
				}
			}
			final := awaitTerminal(t, ctx, api, accepted.Run.ID)
			expectedStatus := "cancelled"
			if graceful {
				expectedStatus = "succeeded"
			}
			if final.Status != expectedStatus || (expectedStatus == "succeeded" && string(final.Outputs["answer"]) != "42") {
				t.Fatalf("shutdown recovery run=%+v", final)
			}
			var attempts, workers int
			var finalRootDeadline, finalNodeDeadline time.Time
			if err := admin.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
    (SELECT count(*) FROM knotra_workers),(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
    (SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND admitted_at IS NOT NULL),
    (SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),execution_deadline,
    (SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='generate') FROM knotra_runs WHERE id=$1`, accepted.Run.ID).Scan(&attempts, &workers, &debits, &operations, &slots, &finalRootDeadline, &finalNodeDeadline); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || workers != 2 || debits != 1 || operations != 1 || slots != 0 || calls.Load() != 1 || !finalRootDeadline.Equal(rootDeadline) || !finalNodeDeadline.Equal(nodeDeadline) {
				t.Fatalf("shutdown recovery attempts/workers/debits/operations/slots/calls=%d/%d/%d/%d/%d/%d root=%v node=%v", attempts, workers, debits, operations, slots, calls.Load(), finalRootDeadline, finalNodeDeadline)
			}
			events := readEventsUntil(t, ctx, api, accepted.Run.ID, "", terminalEvent)
			var terminalEvents int
			if err := admin.Pool.QueryRow(ctx, `SELECT count(*) FROM knotra_events WHERE run_id=$1
                AND document->>'type'='run' AND document->'data'->>'status' IN ('succeeded','failed','cancelled')`, accepted.Run.ID).Scan(&terminalEvents); err != nil {
				t.Fatal(err)
			}
			data, ok := events[len(events)-1].Data.(map[string]any)
			if terminalEvents != 1 || !ok || data["status"] != expectedStatus {
				t.Fatalf("shutdown recovery terminal events=%d SSE=%+v", terminalEvents, events[len(events)-1])
			}

		})
	}
}

func awaitLifecycleBoundary(t *testing.T, ctx context.Context, process *engineProcess, logPath string, reached func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if process != nil {
			select {
			case <-process.finished:
				log, _ := os.ReadFile(logPath)
				t.Fatalf("service exited before lifecycle boundary: %s", log)
			default:
			}
		}
		if reached() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("lifecycle boundary not reached", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertLifecycleSIGKILL(t *testing.T, process *engineProcess) {
	t.Helper()
	if state, ok := process.command.ProcessState.Sys().(syscall.WaitStatus); !ok || state.Signal() != syscall.SIGKILL {
		t.Fatalf("service did not exit from SIGKILL: %v", process.command.ProcessState)
	}
}

func assertLifecycleListenerClosed(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if err == nil {
		_ = connection.Close()
		t.Fatal("service listener still accepts connections at lifecycle boundary")
	}
}
