package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
)

// Execute a real sandbox, then arbitrate its saved outcome using mutation locks.
// Consume native wakes explicitly after arbitration to finish bounded graph steps.
func TestRuntimePhysicalOutcomeArbitratesWithStopAndDeadline(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_HELPER for physical result/stop contention checks")
	}
	for _, mode := range []string{"result_then_cancel", "cancel_then_result", "duplicate_result", "result_then_failure", "failure_then_result", "node_deadline_before_stage", "root_deadline_before_stage", "node_deadline_during_commit", "root_deadline_during_commit"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var modelCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"id":"fixture-model"}`)
					return
				}
				modelCalls.Add(1)
				http.Error(w, "fixture permanent model rejection", http.StatusBadRequest)
			}))
			defer provider.Close()
			plan := resourceCodePlan(t, r, 0)
			spec := &plan.Pipelines[plan.Root].Spec
			node := spec.Nodes["a"]
			report := contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}
			node.Outputs = map[string]contract.Port{"nonce": {Schema: json.RawMessage(`{"type":"string"}`)}, "report": report}
			node.Code = &contract.CodeNode{Command: []string{"python", "-c", `import json,os,pathlib,uuid; nonce=uuid.uuid4().hex; pathlib.Path('report.txt').write_text(nonce); json.dump({'nonce':nonce},open(os.environ['KNOTRA_OUTPUT_JSON'],'w'))`}}
			beforeStage := strings.HasSuffix(mode, "before_stage")
			deadlineCase := strings.Contains(mode, "deadline")
			rootExpiry := strings.HasPrefix(mode, "root_deadline")
			if deadlineCase {
				node.Execution.Timeout = "4s"
			}
			if rootExpiry {
				plan.Profile.Spec.Limits.Timeout = "4s"
			}
			after := node
			after.Dependencies = []string{"a"}
			after.Code = &contract.CodeNode{Command: []string{"python", "-c", `raise RuntimeError('downstream must not execute')`}}
			wait := contract.Node{Type: "human", Human: &contract.HumanNode{Prompt: contract.TextSource{Text: "Hold root open"}}, Outputs: map[string]contract.Port{"approved": {Schema: json.RawMessage(`{"type":"boolean"}`)}}}
			spec.Nodes = map[string]contract.Node{"a": node, "after": after, "wait": wait}
			export := report
			export.Collect, export.Bind = nil, &contract.Binding{From: "nodes.a.outputs.report"}
			spec.Outputs = map[string]contract.Port{"report": export}
			failureCase := strings.HasSuffix(mode, "failure") || mode == "failure_then_result"
			if failureCase {
				plan.Profile.Spec.Limits.MaxConcurrentNodes = 2
				plan.Profile.Spec.Models = map[string]contract.ModelConnection{"cloud": {Provider: "openai", Model: "fixture-model", BaseURL: provider.URL, Auth: map[string]contract.Credential{"key": {Value: new("fixture-key")}}}}
				spec.Models = map[string]contract.Model{"writer": {Connection: "cloud", Requires: []string{"structuredOutput"}}}
				spec.Nodes["fail"] = contract.Node{Type: "llm", LLM: &contract.LLMNode{Model: "writer", Prompt: contract.TextSource{Text: "fixture permanent rejection"}}, Outputs: map[string]contract.Port{"answer": {Schema: json.RawMessage(`{"type":"integer"}`)}}}
			}
			if err := r.Host.Runner.Prepare(ctx, &plan); err != nil {
				t.Fatal(err)
			}
			if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID); err != nil {
				t.Fatal(err)
			}
			id := admitRuntimeRun(t, r, plan)
			claim := func(name string) *execution.Claim {
				t.Helper()
				var owned *execution.Claim
				if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
					if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
						return err
					}
					var err error
					owned, err = r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/"+name), Number: 1}, 1, r.WorkerID)
					return err
				}); err != nil || owned == nil {
					t.Fatalf("claim %s=%+v error=%v", name, owned, err)
				}
				return owned
			}
			owned := claim("a")
			save := func(owned *execution.Claim) execution.Outcome {
				t.Helper()
				result := r.Host.Execute(ctx, owned.Request)
				outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: owned.Ownership, PlanID: owned.PlanID, CompletedAt: time.Now().UTC(), Outputs: result.Outputs, Failure: result.Failure, Artifacts: outcomeArtifacts(result.Outputs)}
				key, err := r.Outcomes.Put(outcome)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := r.Outcomes.Get(key)
				if err != nil {
					t.Fatal(err)
				}
				return stored
			}
			outcome := save(owned)
			if outcome.Failure != nil || len(outcome.Outputs["report"].Artifacts) != 1 {
				t.Fatalf("physical code outcome=%+v", outcome)
			}
			artifact := outcome.Outputs["report"].Artifacts[0]
			var nonce string
			if err := json.Unmarshal(outcome.Outputs["nonce"].JSON, &nonce); err != nil || len(nonce) != 32 {
				t.Fatalf("physical nonce=%q error=%v", nonce, err)
			}
			failure := execution.Outcome{}
			if failureCase {
				failure = save(claim("fail"))
				if failure.Failure == nil || failure.Failure.Code != "MODEL_REQUEST_REJECTED" || modelCalls.Load() != 1 {
					t.Fatalf("physical model rejection=%+v calls=%d", failure.Failure, modelCalls.Load())
				}
			}
			var rootDeadline, nodeDeadline time.Time
			if err := r.Store.Pool.QueryRow(ctx, "SELECT execution_deadline,(SELECT deadline FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='a') FROM knotra_runs WHERE id=$1", id).Scan(&rootDeadline, &nodeDeadline); err != nil {
				t.Fatal(err)
			}
			handler := (&api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}).Handler()
			cancelRun := func() error {
				response := runtimeCommand(t, handler, "/v1/runs/"+id+"/cancel", "cancel-publication", `{}`)
				if response.Code != 202 {
					return fmt.Errorf("cancel=%d %s", response.Code, response.Body.String())
				}
				return nil
			}
			publish := func(value execution.Outcome, recovery bool) error {
				return r.publish(ctx, value, recovery, func(pgx.Tx) error { return nil })
			}
			firstWork := func() error { return publish(outcome, false) }
			secondWork := cancelRun
			if mode == "cancel_then_result" {
				firstWork, secondWork = cancelRun, func() error { return publish(outcome, true) }
			}
			if mode == "duplicate_result" {
				secondWork = func() error { return publish(outcome, true) }
			}
			if mode == "result_then_failure" {
				secondWork = func() error { return publish(failure, false) }
			}
			if mode == "failure_then_result" {
				firstWork, secondWork = func() error { return publish(failure, false) }, func() error { return publish(outcome, true) }
			}
			if deadlineCase {
				secondWork = func() error {
					return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") })
				}
			}
			barrier, err := r.Store.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Release()
			barrierPID := int32(barrier.Conn().PgConn().PID())
			var locked pgx.Tx
			if beforeStage {
				locked, err = barrier.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = locked.Rollback(context.Background()) }()
				if _, err := locked.Exec(ctx, "SELECT id FROM knotra_runs WHERE id=$1 FOR UPDATE", id); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := barrier.Exec(ctx, "SELECT pg_advisory_lock(918273654)"); err != nil {
					t.Fatal(err)
				}
				defer func() { _, _ = barrier.Exec(context.Background(), "SELECT pg_advisory_unlock(918273654)") }()
				table, when := "knotra_execution_attempts", "OLD.state='claimed' AND NEW.state='completed'"
				if mode == "cancel_then_result" {
					table, when = "knotra_runs", "NOT OLD.cancel_requested AND NEW.cancel_requested"
				}
				if _, err := r.Store.Pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION outcome_race_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(918273654); RETURN NEW; END $$;
					CREATE TRIGGER outcome_race_barrier BEFORE UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION outcome_race_barrier()`, table, when)); err != nil {
					t.Fatal(err)
				}
			}
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { first <- firstWork() }()
			waitSQL := "SetAttemptCompleted"
			if mode == "cancel_then_result" {
				waitSQL = "SetRunCancelled"
			}
			if beforeStage {
				waitSQL = "LockExecutionRun"
			}
			var firstPID int32
			await(t, 5*time.Second, func() (bool, error) {
				err := r.Store.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%'||$2||'%' LIMIT 1),0)`, barrierPID, waitSQL).Scan(&firstPID)
				return firstPID != 0, err
			})
			if !beforeStage {
				go func() { second <- secondWork() }()
				waitSQL = "LockExecutionRun"
				if mode == "result_then_cancel" {
					waitSQL = "LockRunCommand"
				}
				await(t, 5*time.Second, func() (bool, error) {
					var blocked bool
					err := r.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%'||$2||'%')`, firstPID, waitSQL).Scan(&blocked)
					return blocked, err
				})
			}
			if deadlineCase {
				await(t, 5*time.Second, func() (bool, error) {
					var expired bool
					err := r.Store.Pool.QueryRow(ctx, "SELECT clock_timestamp() > $1::timestamptz", nodeDeadline).Scan(&expired)
					return expired, err
				})
			}
			if beforeStage {
				if err := r.advanceTx(ctx, locked, plan, id, ""); err != nil {
					t.Fatal(err)
				}
				if err := locked.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				go func() { second <- secondWork() }()
			} else if _, err := barrier.Exec(ctx, "SELECT pg_advisory_unlock(918273654)"); err != nil {
				t.Fatal(err)
			}
			for i, done := range []chan error{first, second} {
				select {
				case err := <-done:
					if beforeStage && i == 0 {
						if !errors.Is(err, store.ErrExecutionOwnership) {
							t.Fatalf("late live publisher error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("publication race did not finish", ctx.Err())
				}
			}
			// Recovery must retain the confirmed evidence without changing the
			// winner, releasing another slot or publishing a losing artifact.
			for range 2 {
				if err := publish(outcome, true); err != nil {
					t.Fatal(err)
				}
			}
			if failureCase {
				if err := publish(failure, true); err != nil {
					t.Fatal(err)
				}
			}
			consumeTestWakes(t, r, plan, id)
			if mode != "duplicate_result" {
				if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
					late, err := r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/after"), Number: 1}, 1, r.WorkerID)
					if late != nil {
						return errors.New("stopped run admitted downstream work")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var nodeState, attemptState, runState, stopCode, afterState, storedNonce string
			var slots, attempts, claimedAfter, tools, models, completedTools, successes, published, resources, sandboxRecords, open, evidence, results int
			var finalRootDeadline, finalNodeDeadline time.Time
			if err := r.Store.Pool.QueryRow(ctx, `SELECT n.state,a.state,r.document->>'status',COALESCE(r.stop_cause->>'code',''),
				(SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='after'),COALESCE(n.outputs->'nonce'->>'json',''),
				(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				(SELECT count(*) FROM knotra_execution_attempts x JOIN knotra_execution_nodes y ON y.run_id=x.run_id AND y.id=x.instance_id WHERE x.run_id=$1 AND y.node_id='after' AND x.owner IS NOT NULL),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='tool'),0),
				COALESCE((SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),0),
				(SELECT count(*) FROM knotra_operations WHERE run_id=$1 AND kind='tool' AND completed),
				(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->'data'->>'nodeId'='a' AND document->'data'->>'status'='succeeded'),
				(SELECT count(*) FROM knotra_artifacts WHERE document->'origin'->>'runId'=$1 AND published),
				(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND state<>'closed'),
				(SELECT count(*) FROM knotra_resources WHERE run_id=$1 AND kind='sandbox'),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND outcome IS NOT NULL AND evidence_key IS NOT NULL),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2 AND result IS NOT NULL),r.execution_deadline,n.deadline
				FROM knotra_runs r JOIN knotra_execution_nodes n ON n.run_id=r.id AND n.id=$2 JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id WHERE r.id=$1`, id, owned.InstanceID).
				Scan(&nodeState, &attemptState, &runState, &stopCode, &afterState, &storedNonce, &slots, &attempts, &claimedAfter, &tools, &models, &completedTools, &successes, &published, &resources, &sandboxRecords, &open, &evidence, &results, &finalRootDeadline, &finalNodeDeadline); err != nil {
				t.Fatal(err)
			}
			won := mode == "result_then_cancel" || mode == "duplicate_result" || mode == "result_then_failure"
			wantNode, wantAttempt, wantRun, wantStop, wantAfter := "cancelled", "cancelled", "cancelled", "CANCELLED", "cancelled"
			wantNonce, wantSuccesses, wantPublished, wantModels, wantAttempts, wantEvidence, wantResults, wantOpen := "", 0, 0, 0, 1, 1, 0, 0
			if won {
				wantNode, wantAttempt, wantNonce, wantSuccesses, wantPublished, wantResults = "succeeded", "completed", nonce, 1, 1, 1
			}
			if mode == "duplicate_result" {
				wantRun, wantStop, wantAfter, wantOpen = "running", "", "ready", 1
				wantAttempts = 2
			}
			if failureCase {
				wantRun, wantStop, wantModels, wantEvidence = "failed", "MODEL_REQUEST_REJECTED", 1, 2
				wantAttempts++
			}
			if deadlineCase {
				wantRun, wantStop = "failed", "DEADLINE_EXCEEDED"
				if !rootExpiry {
					wantNode = "failed"
				}
				if !beforeStage {
					wantAttempt, wantResults = "completed", 1
				}
			}
			if nodeState != wantNode || attemptState != wantAttempt || runState != wantRun || stopCode != wantStop || afterState != wantAfter || storedNonce != wantNonce || slots != 0 || attempts != wantAttempts || claimedAfter != 0 || tools != 1 || models != wantModels || modelCalls.Load() != int32(wantModels) || completedTools != 1 || successes != wantSuccesses || published != wantPublished || resources != 0 || sandboxRecords != 1 || open != wantOpen || evidence != wantEvidence || results != wantResults || !rootDeadline.Equal(finalRootDeadline) || !nodeDeadline.Equal(finalNodeDeadline) {
				t.Fatalf("node/attempt/run=%s/%s/%s cause=%s after=%s nonce=%s slots=%d attempts=%d afterClaims=%d budgets=%d/%d calls=%d journals=%d successes=%d published=%d resources=%d/%d open=%d evidence=%d results=%d deadlines=%v/%v", nodeState, attemptState, runState, stopCode, afterState, storedNonce, slots, attempts, claimedAfter, tools, models, modelCalls.Load(), completedTools, successes, published, resources, sandboxRecords, open, evidence, results, rootDeadline.Equal(finalRootDeadline), nodeDeadline.Equal(finalNodeDeadline))
			}
			retained, err := r.Outcomes.Get(owned.OutcomeKey)
			if err != nil || string(retained.Outputs["nonce"].JSON) != string(outcome.Outputs["nonce"].JSON) || len(retained.Artifacts) != 1 || retained.Artifacts[0].ID != artifact.ID {
				t.Fatalf("original outcome evidence was not retained: error=%v outcome=%+v", err, retained)
			}
			data, err := r.Host.Artifacts.Get(ctx, artifact.ID)
			if err != nil || string(data) != nonce {
				t.Fatalf("original immutable bytes were not retained: error=%v bytes=%q", err, data)
			}
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/artifacts/"+artifact.ID+"/content", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if won {
				if response.Code != 200 || response.Body.String() != nonce {
					t.Fatalf("confirmed artifact changed: HTTP %d body=%q", response.Code, response.Body.String())
				}
			} else if response.Code != 404 {
				t.Fatalf("losing artifact exposed: HTTP %d", response.Code)
			}
			run, err := r.Store.Run(ctx, id)
			if err != nil || len(run.Outputs) != 0 || len(run.Artifacts) != 0 {
				t.Fatalf("incomplete/stopped run exported artifacts: run=%+v error=%v", run, err)
			}
			t.Log("physical code/journal/immutable envelope executed once; contended publication kept one node winner, correct artifact visibility, original deadlines and slot accounting without downstream execution")
		})
	}
}
