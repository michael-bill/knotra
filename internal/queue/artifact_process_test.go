//go:build unix

package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRuntimeArtifactPublicationProcessCrashBoundaries(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_RUNTIME_RECOVERY") != "1" || os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_RUNTIME_RECOVERY and KNOTRA_TEST_HELPER for artifact publication SIGKILL checks")
	}
	for _, mode := range []string{"artifact_bytes_saved", "artifact_registered_before_journal", "artifact_publication_before_commit", "delivery_completion_before_commit", "next_delivery_before_commit", "result_committed"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			directory := t.TempDir()
			r.Host.Runner.WorkDir = filepath.Join(directory, "work")
			r.Host.Artifacts.Root = filepath.Join(directory, "artifacts")
			files, err := execution.OpenOutcomeFiles(filepath.Join(directory, "outcomes"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = files.Close() }()
			r.Outcomes = files
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"downstream confirmed\"}"},"done":true,"done_reason":"stop"}`)
			}))
			defer provider.Close()
			plan := resourceCodePlan(t, r, 0)
			plan.Profile.Spec.Limits.Timeout = "3m"
			plan.Profile.Spec.Models = runtimePlan(provider.URL).Profile.Spec.Models
			spec := &plan.Pipelines[plan.Root].Spec
			spec.Models = runtimePlan(provider.URL).Pipelines["main.yaml"].Spec.Models
			report := contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}
			node := spec.Nodes["a"]
			node.Outputs = map[string]contract.Port{"nonce": {Schema: json.RawMessage(`{"type":"string"}`)}, "report": report}
			node.Execution.Timeout = "90s"
			node.Code = &contract.CodeNode{Command: []string{"python", "-c", `import json,os,pathlib,uuid; nonce=uuid.uuid4().hex; pathlib.Path('report.txt').write_text(nonce); json.dump({'nonce':nonce},open(os.environ['KNOTRA_OUTPUT_JSON'],'w'))`}}
			after := runtimePlan(provider.URL).Pipelines["main.yaml"].Spec.Nodes["c"]
			after.Inputs = map[string]contract.Port{"previous": {Schema: json.RawMessage(`{"type":"string"}`), Bind: &contract.Binding{From: "nodes.a.outputs.nonce"}}}
			spec.Nodes = map[string]contract.Node{"a": node, "after": after}
			report.Collect, report.Bind = nil, &contract.Binding{From: "nodes.a.outputs.report"}
			spec.Outputs = map[string]contract.Port{"report": report, "nonce": {Schema: json.RawMessage(`{"type":"string"}`), Bind: &contract.Binding{From: "nodes.a.outputs.nonce"}}, "result": {Schema: json.RawMessage(`{"type":"string"}`), Bind: &contract.Binding{From: "nodes.after.outputs.answer"}}}
			id := admitRuntimeRun(t, r, plan)
			instance := execution.StableID("n", id+"/root/a")
			afterInstance := execution.StableID("n", id+"/root/after")
			dsn, err := url.Parse(r.Store.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			application := "knotra-artifact-crash-" + id
			query := dsn.Query()
			query.Set("application_name", application)
			dsn.RawQuery = query.Encode()
			start := func(childMode string) *exec.Cmd {
				t.Helper()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeProcessCrashBoundaries$", "-test.timeout=2m")
				cmd.Env = append(os.Environ(), "KNOTRA_RUNTIME_CHILD=1", "KNOTRA_RUNTIME_MODE="+childMode, "KNOTRA_RUNTIME_DSN="+dsn.String(), "KNOTRA_RUNTIME_DIRECTORY="+directory, "KNOTRA_RUNTIME_RUN="+id, "KNOTRA_RUNTIME_TIMEOUT=2m", "KNOTRA_RUNTIME_HOST="+r.HostID, "KNOTRA_RUNTIME_HELPER="+r.Host.Runner.HelperPath, "KNOTRA_RUNTIME_WAIT_CLEANUP=1")
				log, err := os.Create(filepath.Join(directory, childMode+".log"))
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stdout, cmd.Stderr = log, log
				err = cmd.Start()
				_ = log.Close()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
					if t.Failed() {
						data, _ := os.ReadFile(filepath.Join(directory, childMode+".log"))
						t.Logf("%s process: %s", childMode, data)
					}
				})
				return cmd
			}
			var barrier *pgx.Conn
			var table string
			if mode != "result_committed" {
				barrier, err = pgx.Connect(ctx, dsn.String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Close(context.Background()) }()
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273655)"); err != nil {
					t.Fatal(err)
				}
				table = "knotra_artifacts"
				event, when := "BEFORE INSERT", "true"
				switch mode {
				case "artifact_registered_before_journal":
					table, event, when = "knotra_operations", "BEFORE UPDATE", "NEW.kind='tool' AND NEW.completed AND NOT OLD.completed"
				case "artifact_publication_before_commit":
					event, when = "AFTER UPDATE", "NEW.published AND NOT OLD.published"
				case "delivery_completion_before_commit":
					table, event, when = "river_job", "AFTER UPDATE", "NEW.state='completed' AND OLD.state='running' AND NEW.kind='knotra_execute_v1' AND NEW.args->'attempt'->>'instanceId'='"+instance+"'"
				case "next_delivery_before_commit":
					table, event, when = "river_job", "BEFORE INSERT", "NEW.kind='knotra_execute_v1' AND NEW.args->'attempt'->>'instanceId'='"+afterInstance+"'"
				}
				if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION artifact_crash_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273655); RETURN NEW; END $$; CREATE TRIGGER artifact_crash_barrier %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION artifact_crash_barrier()`, event, table, when)); err != nil {
					t.Fatal(err)
				}
			}
			child := start(mode)
			await(t, 15*time.Second, func() (bool, error) {
				if mode == "result_committed" {
					status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
					return status == "succeeded", err
				}
				var waiting bool
				err := r.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory' AND pid<>pg_backend_pid())`, application).Scan(&waiting)
				return waiting, err
			})
			var rootDeadline, nodeDeadline time.Time
			var key string
			if err := r.Store.Pool.QueryRow(ctx, `SELECT r.execution_deadline,n.deadline,a.outcome_key FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id WHERE r.id=$1 AND n.id=$2`, id, instance).Scan(&rootDeadline, &nodeDeadline, &key); err != nil {
				t.Fatal(err)
			}
			var sandbox string
			if err := r.Store.Pool.QueryRow(ctx, "SELECT id FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'", id).Scan(&sandbox); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_, _ = exec.CommandContext(cleanup, "docker", "rm", "--force", "sandbox-"+sandbox).CombinedOutput()
			})
			entries, err := os.ReadDir(r.Host.Artifacts.Root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("immutable artifact files=%d error=%v", len(entries), err)
			}
			hash := entries[0].Name()
			nonce, err := os.ReadFile(filepath.Join(r.Host.Artifacts.Root, hash))
			sum := sha256.Sum256(nonce)
			if err != nil || len(nonce) != 32 || hex.EncodeToString(sum[:]) != hash {
				t.Fatalf("artifact bytes=%q hash=%s error=%v", nonce, hash, err)
			}
			unknown := mode == "artifact_bytes_saved" || mode == "artifact_registered_before_journal"
			outcome, envelopeErr := files.Get(key)
			if unknown {
				if !os.IsNotExist(envelopeErr) {
					t.Fatalf("pre-journal envelope exists: %v", envelopeErr)
				}
			} else if envelopeErr != nil || outcome.Failure != nil || len(outcome.Artifacts) != 1 || outcome.Artifacts[0].SHA256 != hash || string(outcome.Outputs["nonce"].JSON) != `"`+string(nonce)+`"` {
				t.Fatalf("durable envelope=%+v error=%v", outcome, envelopeErr)
			}
			var artifactID string
			if mode != "artifact_bytes_saved" {
				if err := r.Store.Pool.QueryRow(ctx, "SELECT id FROM knotra_artifacts WHERE document->'origin'->>'runId'=$1", id).Scan(&artifactID); err != nil {
					t.Fatal(err)
				}
			}
			handler := (&api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}).Handler()
			checkPublication := func(committed, downstream bool) {
				t.Helper()
				var published, evidence, results, successes, next int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM knotra_artifacts WHERE document->'origin'->>'runId'=$1 AND published),
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2 AND outcome IS NOT NULL),
					(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2 AND result IS NOT NULL AND result->'failure' IS NULL),
					(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->'data'->>'nodeId'='a' AND document->'data'->>'status'='succeeded'),
					(SELECT count(*) FROM river_job WHERE kind='knotra_execute_v1' AND args->'attempt'->>'instanceId'=$3)`, id, instance, afterInstance).Scan(&published, &evidence, &results, &successes, &next); err != nil {
					t.Fatal(err)
				}
				want := 0
				if committed {
					want = 1
				}
				wantNext := 0
				if downstream {
					wantNext = 1
				}
				if published != want || evidence != want || results != want || successes != want || next != wantNext || calls.Load() != int32(wantNext) {
					t.Fatalf("publication/evidence/result/event/next=%d/%d/%d/%d/%d downstream calls=%d want=%d/%d", published, evidence, results, successes, next, calls.Load(), want, wantNext)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/artifacts/"+artifactID+"/content", nil))
				if committed {
					if response.Code != 200 || response.Body.String() != string(nonce) {
						t.Fatalf("published artifact HTTP %d body=%q", response.Code, response.Body.String())
					}
				} else if artifactID != "" && response.Code != 404 {
					t.Fatalf("private artifact exposed: HTTP %d", response.Code)
				}
			}
			// A dependent execute delivery is admitted in the next bounded graph
			// step. Its predecessor and publication wake have already committed.
			committed := mode == "result_committed" || mode == "next_delivery_before_commit"
			checkPublication(committed, mode == "result_committed")
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if err := child.Wait(); !errors.As(err, &exit) {
				t.Fatalf("child was not killed: %v", err)
			}
			if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not receive SIGKILL: %v", exit)
			}
			if barrier != nil {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273655)"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.Pool.Exec(ctx, "DROP TRIGGER artifact_crash_barrier ON "+table); err != nil {
					t.Fatal(err)
				}
			}
			checkPublication(committed, mode == "result_committed")
			replacement := start("recover")
			if err := replacement.Wait(); err != nil {
				t.Fatal("replacement failed", err)
			}
			run, err := r.Store.Run(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if unknown {
				if run.Status != "waiting_resolution" || len(run.Outputs) != 0 || len(run.Artifacts) != 0 {
					t.Fatalf("unconfirmed artifact became a result: %+v", run)
				}
				checkPublication(false, false)
				var lost string
				var requests int
				if err := r.Store.Pool.QueryRow(ctx, `SELECT result->'failure'->>'code',(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open') FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2`, id, instance).Scan(&lost, &requests); err != nil || lost != "OUTCOME_UNKNOWN" || requests != 1 {
					t.Fatalf("lost attempt failure/request=%s/%d error=%v", lost, requests, err)
				}
				if w := runtimeCommand(t, handler, "/v1/runs/"+id+"/cancel", "cancel-unknown", `{}`); w.Code != 202 {
					t.Fatalf("cancel unresolved run: HTTP %d %s", w.Code, w.Body.String())
				}
				if status, err := store.ReadRunStatus(ctx, r.Store.Pool, id); err != nil || status != "cancelled" {
					t.Fatalf("unresolved run did not cancel: %s %v", status, err)
				}
			} else {
				if run.Status != "succeeded" || string(run.Outputs["nonce"]) != `"`+string(nonce)+`"` || string(run.Outputs["result"]) != `"downstream confirmed"` || len(run.Artifacts) != 1 || run.Artifacts[0].ID != artifactID {
					t.Fatalf("confirmed artifact/output changed: %+v", run)
				}
				checkPublication(true, true)
			}
			var attempts, tools, models, slots, resources, sandboxes int
			var finalRootDeadline, finalNodeDeadline time.Time
			if err := r.Store.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed'),
				(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'),
				r.execution_deadline,n.deadline FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id AND n.id=$2 WHERE r.id=$1`, id, instance).Scan(&attempts, &tools, &models, &slots, &resources, &sandboxes, &finalRootDeadline, &finalNodeDeadline); err != nil {
				t.Fatal(err)
			}
			wantAttempts, wantModels := 2, 1
			if unknown {
				wantAttempts, wantModels = 1, 0
			}
			if attempts != wantAttempts || tools != 1 || models != wantModels || slots != 0 || resources != 0 || sandboxes != 1 || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) {
				t.Fatalf("attempts/debits=%d/%d/%d slots/resources/sandboxes=%d/%d/%d deadlines=%v/%v", attempts, tools, models, slots, resources, sandboxes, rootDeadline.Equal(finalRootDeadline), nodeDeadline.Equal(finalNodeDeadline))
			}
			if output, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name=^/sandbox-"+sandbox+"$", "--format", "{{.Names}}").CombinedOutput(); err != nil || len(output) != 0 {
				t.Fatalf("recovery retained a physical sandbox: %s %v", output, err)
			}
			data, err := os.ReadFile(filepath.Join(r.Host.Artifacts.Root, hash))
			if err != nil || string(data) != string(nonce) {
				t.Fatalf("original immutable bytes changed: %q %v", data, err)
			}
			t.Log("SIGKILL preserved private artifact bytes and atomic result/event/publication/next delivery; recovery retained the nonce, budgets, deadlines and one sandbox generation")
		})
	}
}
