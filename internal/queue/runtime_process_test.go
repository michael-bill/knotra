//go:build unix

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/executor"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

// Each child is the real Runtime/Host, with its own heartbeat and River client.
// PostgreSQL barriers stop production writes at the named crash boundaries.
func runtimeProcessChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := store.Open(ctx, os.Getenv("KNOTRA_RUNTIME_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	directory := os.Getenv("KNOTRA_RUNTIME_DIRECTORY")
	files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	runner := &adapters.Runner{HelperPath: os.Getenv("KNOTRA_RUNTIME_HELPER"), WorkDir: filepath.Join(directory, "work")}
	defer func() { _ = runner.Close() }()
	if os.Getenv("KNOTRA_RUNTIME_MODE") == "operation_admitted_before_send" {
		// The native HTTP transport is reached only after physical-call admission
		// commits. Hold its dial before a request can reach the literal provider.
		runner.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if err := writeSignal(directory, "admitted"); err != nil {
				return nil, err
			}
			if err := waitSignal(ctx, directory, "allow-dial"); err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}}
	}
	hostID := os.Getenv("KNOTRA_RUNTIME_HOST")
	if hostID == "" {
		hostID = "process-test"
	}
	r := &Runtime{Store: s, Outcomes: files, WorkerID: uuid.NewString(), HostID: hostID, Host: &executor.Host{Store: s, Runner: runner, Artifacts: store.Artifacts{Store: s, Root: filepath.Join(directory, "artifacts")}}}
	handlers := r.Handlers()
	if os.Getenv("KNOTRA_RUNTIME_MODE") == "fetched_before_claim" {
		handlers.Execute = func(ctx context.Context, job *river.Job[ExecuteArgs]) error {
			if err := writeSignal(directory, "fetched"); err != nil {
				return err
			}
			if err := waitSignal(ctx, directory, "allow-claim"); err != nil {
				return err
			}
			return r.execute(ctx, job)
		}
	}
	schema := s.Pool.Config().ConnConfig.RuntimeParams["search_path"]
	timeout, err := time.ParseDuration(os.Getenv("KNOTRA_RUNTIME_TIMEOUT"))
	if err != nil {
		t.Fatal(err)
	}
	r.Client, err = New(s.Pool, Config{EngineID: s.EngineID, HostID: r.HostID, Schema: schema, ExecutionWorkers: 2, ExecutionTimeout: timeout}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := r.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	if os.Getenv("KNOTRA_RUNTIME_MODE") == "recover" {
		await(t, 2*time.Minute, func() (bool, error) {
			status, err := store.ReadRunStatus(ctx, s.Pool, os.Getenv("KNOTRA_RUNTIME_RUN"))
			return protocol.Terminal(status) || status == "waiting_resolution", err
		})
		if os.Getenv("KNOTRA_RUNTIME_WAIT_CLEANUP") == "1" {
			await(t, 30*time.Second, func() (bool, error) {
				var open int
				err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed'", os.Getenv("KNOTRA_RUNTIME_RUN")).Scan(&open)
				return open == 0, err
			})
		}
		return
	}
	<-ctx.Done()
	t.Fatal("child did not reach its crash boundary")
}

func TestRuntimeProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_RUNTIME_CHILD") == "1" {
		runtimeProcessChild(t)
		return
	}
	if os.Getenv("KNOTRA_TEST_RUNTIME_RECOVERY") != "1" {
		t.Skip("set KNOTRA_TEST_RUNTIME_RECOVERY=1 for actual Runtime SIGKILL checks")
	}
	modes := []string{"fetched_before_claim", "ownership_claimed_before_intent", "prepared_intent_before_admission", "operation_admitted_before_send", "external_accepted_before_journal", "response_journaled_before_envelope", "envelope_saved_before_result_commit", "result_committed"}
	if os.Getenv("KNOTRA_TEST_RUNTIME_OUTAGE") == "1" {
		modes = append(modes, "database_outage_saved_envelope")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			duration := 90 * time.Second
			if mode == "database_outage_saved_envelope" {
				duration = 8 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			directory := t.TempDir()
			files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = files.Close() }()
			r.Outcomes = files
			r.Host.Artifacts.Root = filepath.Join(directory, "artifacts")
			var calls atomic.Int32
			release := make(chan struct{})
			var releaseOnce sync.Once
			answer := strings.Repeat("complete-output-", 10000)
			content, _ := json.Marshal(map[string]string{"answer": answer})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if mode == "external_accepted_before_journal" {
					if err := writeSignal(directory, "accepted"); err != nil {
						t.Error(err)
					}
					select {
					case <-release:
					case <-req.Context().Done():
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "done": true, "done_reason": "stop"})
			}))
			defer server.Close()
			defer releaseOnce.Do(func() { close(release) })
			plan := deliveryPlan(server.URL)
			plan.Profile.Spec.Limits.Timeout = "3m"
			plan.Pipelines[plan.Root].Spec.Defaults.Execution.Timeout = "90s"
			beforeSend := mode == "ownership_claimed_before_intent" || mode == "prepared_intent_before_admission" || mode == "operation_admitted_before_send"
			if beforeSend {
				node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
				node.Execution.Retry = &contract.Retry{MaxAttempts: 2, Backoff: "1ms"}
				plan.Pipelines[plan.Root].Spec.Nodes["a"] = node
			}
			executionTimeout := "2m"
			if mode == "database_outage_saved_envelope" {
				plan.Profile.Spec.Limits.Timeout = "9m"
				plan.Pipelines[plan.Root].Spec.Defaults.Execution.Timeout = "7m"
				executionTimeout = "10m"
			}
			id := admitRuntimeRun(t, r, plan)
			schema := r.Store.Pool.Config().ConnConfig.RuntimeParams["search_path"]
			dsn, err := url.Parse(os.Getenv("KNOTRA_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			query := dsn.Query()
			query.Set("search_path", schema)
			query.Set("application_name", "knotra-runtime-crash-"+id)
			dsn.RawQuery = query.Encode()
			directDSN := dsn.String()
			var proxy *outageProxy
			if mode == "database_outage_saved_envelope" {
				config := r.Store.Pool.Config().ConnConfig
				proxy = newOutageProxy(t, net.JoinHostPort(config.Host, fmt.Sprint(config.Port)))
				dsn.Host = proxy.listener.Addr().String()
			}
			start := func(childMode string) *exec.Cmd {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRuntimeProcessCrashBoundaries$", "-test.timeout=2m")
				cmd.Env = append(os.Environ(), "KNOTRA_RUNTIME_CHILD=1", "KNOTRA_RUNTIME_MODE="+childMode, "KNOTRA_RUNTIME_DSN="+dsn.String(), "KNOTRA_RUNTIME_DIRECTORY="+directory, "KNOTRA_RUNTIME_RUN="+id, "KNOTRA_RUNTIME_TIMEOUT="+executionTimeout)
				log, err := os.Create(filepath.Join(directory, childMode+".log"))
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stdout = log
				cmd.Stderr = log
				if err := cmd.Start(); err != nil {
					_ = log.Close()
					t.Fatal(err)
				}
				_ = log.Close()
				t.Cleanup(func() { _ = cmd.Process.Kill() })
				return cmd
			}
			// A session lock blocks precisely the chosen committed-journal or envelope
			// boundary. Killing the process releases its transaction and domain locks.
			var barrier *pgx.Conn
			var barrierTable string
			if mode == "ownership_claimed_before_intent" || mode == "prepared_intent_before_admission" || mode == "response_journaled_before_envelope" || mode == "envelope_saved_before_result_commit" || mode == "database_outage_saved_envelope" {
				barrier, err = pgx.Connect(ctx, directDSN)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273645)"); err != nil {
					t.Fatal(err)
				}
				table, when, event := "knotra_events", "NEW.document->>'type'='model.completed'", "INSERT"
				switch mode {
				case "envelope_saved_before_result_commit":
					table, when, event = "knotra_execution_attempts", "NEW.outcome IS NOT NULL AND OLD.outcome IS NULL", "UPDATE"
				case "ownership_claimed_before_intent":
					table, when = "knotra_operations", "NEW.kind='model'"
				case "prepared_intent_before_admission":
					table, when, event = "knotra_operations", "NEW.kind='model' AND OLD.admitted_at IS NULL AND NEW.admitted_at IS NOT NULL", "UPDATE"
				}
				barrierTable = table
				if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273645); RETURN NEW; END $$; CREATE TRIGGER crash_barrier BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION crash_barrier()`, event, table, when)); err != nil {
					t.Fatal(err)
				}
			}
			child := start(mode)
			switch mode {
			case "fetched_before_claim":
				if err := waitSignal(ctx, directory, "fetched"); err != nil {
					t.Fatal(err)
				}
			case "external_accepted_before_journal":
				if err := waitSignal(ctx, directory, "accepted"); err != nil {
					t.Fatal(err)
				}
			case "operation_admitted_before_send":
				if err := waitSignal(ctx, directory, "admitted"); err != nil {
					t.Fatal(err)
				}
			case "ownership_claimed_before_intent", "prepared_intent_before_admission", "response_journaled_before_envelope", "envelope_saved_before_result_commit", "database_outage_saved_envelope":
				await(t, 15*time.Second, func() (bool, error) {
					var waiting bool
					err := r.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event='advisory' AND application_name=$1 AND query LIKE '%knotra_%' AND pid<>pg_backend_pid())`, "knotra-runtime-crash-"+id).Scan(&waiting)
					return waiting, err
				})
			case "result_committed":
				await(t, 20*time.Second, func() (bool, error) {
					status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
					return status == "succeeded", err
				})
			}
			var instance string
			var deadline time.Time
			var key *string
			if err := r.Store.Pool.QueryRow(ctx, "SELECT n.id,n.deadline,a.outcome_key FROM knotra_execution_nodes n JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id WHERE n.run_id=$1", id).Scan(&instance, &deadline, &key); err != nil {
				t.Fatal(err)
			}
			if mode == "fetched_before_claim" {
				var state string
				var effects, reservations int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM knotra_operations WHERE run_id=$1),(SELECT count(*) FROM knotra_execution_slots WHERE run_id=$1) FROM knotra_execution_attempts WHERE run_id=$1`, id).Scan(&state, &effects, &reservations); err != nil || state != "ready" || key != nil || effects != 0 || reservations != 0 || calls.Load() != 0 {
					t.Fatalf("preclaim state=%s key=%v effects=%d reservations=%d calls=%d error=%v", state, key, effects, reservations, calls.Load(), err)
				}
			}
			if beforeSend {
				var state string
				var operations, admitted, budget, slots int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT state,
					(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='model'),
					(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='model' AND admitted_at IS NOT NULL),
					COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
					(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1)
					FROM knotra_execution_attempts WHERE run_id=$1`, id).Scan(&state, &operations, &admitted, &budget, &slots); err != nil {
					t.Fatal(err)
				}
				wantOperations, wantAdmitted := 1, 0
				switch mode {
				case "ownership_claimed_before_intent":
					wantOperations = 0
				case "operation_admitted_before_send":
					wantAdmitted = 1
				}
				if state != "claimed" || key == nil || operations != wantOperations || admitted != wantAdmitted || budget != wantAdmitted || slots != 1 || calls.Load() != 0 {
					t.Fatalf("before-send claim/key=%s/%v operations/admitted/budget/slots/calls=%d/%d/%d/%d/%d", state, key, operations, admitted, budget, slots, calls.Load())
				}
				if _, err := files.Get(*key); !os.IsNotExist(err) {
					t.Fatalf("unexecuted claim has an envelope: %v", err)
				}
			}
			if mode == "external_accepted_before_journal" {
				var completed bool
				if err := r.Store.Pool.QueryRow(ctx, "SELECT completed FROM knotra_operations WHERE run_id=$1 AND kind='model'", id).Scan(&completed); err != nil || completed {
					t.Fatalf("accepted effect already journaled=%v error=%v", completed, err)
				}
				if _, err := files.Get(*key); !os.IsNotExist(err) {
					t.Fatalf("accepted effect already has envelope: %v", err)
				}
			}
			if mode == "response_journaled_before_envelope" || mode == "database_outage_saved_envelope" {
				var completed bool
				if err := r.Store.Pool.QueryRow(ctx, "SELECT completed FROM knotra_operations WHERE run_id=$1 AND kind='model'", id).Scan(&completed); err != nil || !completed {
					t.Fatalf("response journal complete=%v error=%v", completed, err)
				}
				if _, err := files.Get(*key); !os.IsNotExist(err) {
					t.Fatalf("envelope appeared before crash: %v", err)
				}
			}
			if mode == "envelope_saved_before_result_commit" {
				if _, err := files.Get(*key); err != nil {
					t.Fatal(err)
				}
			}
			var outageStart time.Time
			if proxy != nil {
				outageStart = time.Now()
				proxy.setOnline(false)
				await(t, 10*time.Second, func() (bool, error) {
					_, err := files.Get(*key)
					if os.IsNotExist(err) {
						return false, nil
					}
					return err == nil, err
				})
				envelope, err := files.Get(*key)
				if err != nil || envelope.Failure != nil {
					t.Fatalf("outage envelope failure=%+v error=%v", envelope.Failure, err)
				}
				var published bool
				if err := r.Store.Pool.QueryRow(ctx, "SELECT outcome IS NOT NULL FROM knotra_execution_attempts WHERE run_id=$1", id).Scan(&published); err != nil || published {
					t.Fatalf("publication during outage=%v error=%v", published, err)
				}
				t.Log("real Runtime saved confirmed output while its PostgreSQL connections were unavailable")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			exit := &exec.ExitError{}
			ok := errors.As(err, &exit)
			if !ok {
				t.Fatalf("child did not exit from a signal: %v", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child was not killed: %v", err)
			}
			releaseOnce.Do(func() { close(release) })
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273645)"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER crash_barrier ON "+barrierTable); err != nil {
					t.Fatal(err)
				}
			}
			if proxy != nil {
				timer := time.NewTimer(time.Until(outageStart.Add(6 * time.Minute)))
				select {
				case <-ctx.Done():
					timer.Stop()
					t.Fatal(ctx.Err())
				case <-timer.C:
				}
				proxy.setOnline(true)
				t.Logf("restored connectivity after %s without aging domain leases, deadlines or River jobs", time.Since(outageStart))
			}
			replacement := start("recover")
			if err := replacement.Wait(); err != nil {
				log, _ := os.ReadFile(filepath.Join(directory, "recover.log"))
				t.Fatalf("replacement failed: %v\n%s", err, log)
			}
			run, err := r.Store.Run(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			unknown := beforeSend || mode == "external_accepted_before_journal" || mode == "response_journaled_before_envelope"
			if unknown {
				if run.Status != "waiting_resolution" {
					t.Fatalf("lost outcome status=%s", run.Status)
				}
				srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
				decision := map[string]any{"outcome": "succeeded", "evidence": "literal provider fixture verified", "outputs": map[string]string{"answer": answer}}
				if beforeSend {
					if calls.Load() != 0 {
						t.Fatal("expired claim automatically called the provider before an operator decision")
					}
					decision = map[string]any{"outcome": "not_started", "evidence": "physical transport fixture and provider verified zero generation requests"}
				}
				body, _ := json.Marshal(decision)
				if w := runtimeCommand(t, srv.Handler(), "/v1/runs/"+id+"/instances/"+instance+"/resolve", "verified", string(body)); w.Code != 202 {
					t.Fatalf("resolution=%d %s", w.Code, w.Body.String())
				}
				if beforeSend {
					resumed := start("recover")
					if err := resumed.Wait(); err != nil {
						log, _ := os.ReadFile(filepath.Join(directory, "recover.log"))
						t.Fatalf("operator-authorized retry failed: %v\n%s", err, log)
					}
				}
				run, err = r.Store.Run(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			var result string
			if err := json.Unmarshal(run.Outputs["result"], &result); err != nil || result != answer || run.Status != "succeeded" {
				t.Fatalf("recovered status=%s full output bytes=%d error=%v", run.Status, len(result), err)
			}
			var attempts, number, budget, slots, successes, workers int
			var finalDeadline time.Time
			if err := r.Store.Pool.QueryRow(ctx, `SELECT count(*),max(a.number),(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),(SELECT count(*) FROM knotra_workers),max(n.deadline) FROM knotra_execution_attempts a JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id WHERE a.run_id=$1`, id).Scan(&attempts, &number, &budget, &slots, &successes, &workers, &finalDeadline); err != nil {
				t.Fatal(err)
			}
			wantAttempts, wantBudget, wantWorkers := 1, 1, 2
			if beforeSend {
				wantAttempts, wantWorkers = 2, 3
				if mode == "operation_admitted_before_send" {
					wantBudget = 2
				}
			}
			if calls.Load() != 1 || attempts != wantAttempts || number != wantAttempts || budget != wantBudget || slots != 0 || successes != 1 || workers != wantWorkers || !deadline.Equal(finalDeadline) {
				t.Fatalf("calls=%d attempts=%d number=%d budget=%d slots=%d successes=%d workers=%d deadline=%v", calls.Load(), attempts, number, budget, slots, successes, workers, finalDeadline.Equal(deadline))
			}
			t.Log("SIGKILL recovered with one physical call, expected attempt/debit counts, original deadline and complete outputs")
		})
	}
}
