package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/adapters"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/executor"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	db := newTestDatabase(t, 8)
	dsn := os.Getenv("KNOTRA_TEST_DATABASE_URL")
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", db.schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + db.schema
	}
	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := Migrate(context.Background(), db.pool, db.schema); err != nil {
		t.Fatal(err)
	}
	files, err := execution.OpenOutcomeFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	runner := &adapters.Runner{}
	t.Cleanup(func() { _ = runner.Close() })
	r := &Runtime{Store: s, Host: &executor.Host{Store: s, Runner: runner, Artifacts: store.Artifacts{Store: s, Root: t.TempDir()}},
		Outcomes: files, WorkerID: uuid.NewString(), HostID: "runtime-test"}
	r.Client = newTestClient(t, db, s.EngineID, r.HostID, time.Minute, r.Handlers())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return r
}

func runtimePlan(endpoint string) contract.Plan {
	port := contract.Port{Schema: json.RawMessage(`{"type":"string"}`)}
	node := contract.Node{Type: "llm", Outputs: map[string]contract.Port{"answer": port},
		LLM: &contract.LLMNode{Model: "model", Prompt: contract.TextSource{Text: "Return answer."}}}
	dependent := node
	dependent.Dependencies = []string{"a"}
	input := port
	input.Bind = &contract.Binding{From: "nodes.a.outputs.answer"}
	dependent.Inputs = map[string]contract.Port{"previous": input}
	export := port
	export.Bind = &contract.Binding{From: "nodes.c.outputs.answer"}
	return contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "main.yaml", Digest: "runtime-frozen-plan",
		Pipelines: map[string]*contract.Pipeline{"main.yaml": {Spec: contract.Spec{
			Graph:    contract.Graph{Nodes: map[string]contract.Node{"a": node, "b": node, "c": dependent}, Outputs: map[string]contract.Port{"result": export}},
			Defaults: contract.Defaults{Execution: contract.Execution{Timeout: "30s"}}, Models: map[string]contract.Model{"model": {Connection: "local"}}}}},
		Profile: contract.Profile{Spec: contract.ProfileSpec{Limits: contract.Limits{Timeout: "1m", MaxConcurrentNodes: 1, MaxNodeInstances: 100, MaxModelCalls: 100, MaxToolCalls: 100},
			Models: map[string]contract.ModelConnection{"local": {Provider: "ollama", Model: "fixture", BaseURL: endpoint}}}}}
}

func admitRuntimeRun(t *testing.T, r *Runtime, plan contract.Plan, values ...contract.Values) string {
	inputs := contract.Values{}
	if len(values) > 0 {
		inputs = values[0]
	}
	t.Helper()
	definition := protocol.Definition{ID: uuid.NewString(), PackageDigest: uuid.NewString(), Name: "runtime-test"}
	run := protocol.Run{ID: uuid.NewString(), DefinitionID: definition.ID, Status: "pending"}
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := store.PutDefinition(ctx, tx, definition); err != nil {
			return err
		}
		_, err := store.PutRiverRun(ctx, tx, run, plan, inputs, r.Wake)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func TestRuntimePublishesFullDAGAndRedispatchesCapacityMiss(t *testing.T) {
	r := newTestRuntime(t)
	var calls atomic.Int32
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	answer := strings.Repeat("complete-output-", 10000)
	content, _ := json.Marshal(map[string]string{"answer": answer})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/chat" || req.Method != http.MethodPost {
			t.Errorf("unexpected physical request %s %s", req.Method, req.URL.Path)
		}
		if calls.Add(1) == 1 {
			close(blocked)
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "done": true, "done_reason": "stop"})
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, runtimePlan(server.URL))
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("runtime did not execute its admitted leaf")
	}
	await(t, 10*time.Second, func() (bool, error) {
		var missed bool
		err := r.Store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knotra_execution_attempts WHERE run_id=$1 AND state='ready' AND dispatch_pending)", id).Scan(&missed)
		return missed, err
	})
	once.Do(func() { close(release) })
	await(t, 15*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	var result string
	if err := json.Unmarshal(run.Outputs["result"], &result); err != nil || result != answer {
		t.Fatalf("full export changed: bytes=%d error=%v", len(result), err)
	}
	var used, slots, outcomes, succeeded, outbox, redispatched int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
		(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND state='completed' AND outcome IS NOT NULL AND number=1),
		(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded'),
		(SELECT count(*) FROM knotra_outbox WHERE run_id=$1),
		(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND dispatch_generation>1)`, id).
		Scan(&used, &slots, &outcomes, &succeeded, &outbox, &redispatched); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || used != 3 || slots != 0 || outcomes != 3 || succeeded != 3 || outbox != 0 || redispatched < 1 {
		t.Fatalf("calls=%d budget=%d slots=%d outcomes=%d successes=%d temporalCommands=%d redispatched=%d", calls.Load(), used, slots, outcomes, succeeded, outbox, redispatched)
	}
	var previous string
	var truncated bool
	if err := r.Store.Pool.QueryRow(ctx, `SELECT n.inputs->'previous'->>'json', (i.document->>'dataTruncated')::boolean
		FROM knotra_execution_nodes n JOIN knotra_instances i ON i.run_id=n.run_id AND i.id=n.id WHERE n.run_id=$1 AND n.node_id='c'`, id).Scan(&previous, &truncated); err != nil {
		t.Fatal(err)
	}
	if previous != answer || !truncated {
		t.Fatalf("downstream consumed observation instead of full value: bytes=%d truncated=%v", len(previous), truncated)
	}
	queueIdle := func() (bool, error) {
		var pending int
		err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE state NOT IN ('completed','cancelled','discarded')").Scan(&pending)
		return pending == 0, err
	}
	await(t, 5*time.Second, queueIdle)
	var eventsBefore int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_events WHERE run_id=$1", id).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	domainSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := r.Store.Pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'run',(SELECT document::jsonb FROM knotra_runs WHERE id=$1),
			'nodes',(SELECT jsonb_agg(to_jsonb(n) ORDER BY id) FROM knotra_execution_nodes n WHERE run_id=$1),
			'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY instance_id,number) FROM knotra_execution_attempts a WHERE run_id=$1),
			'budgets',(SELECT jsonb_agg(to_jsonb(b) ORDER BY scope,kind) FROM knotra_budgets b WHERE run_id=$1),
			'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM knotra_events e WHERE run_id=$1))::text`, id).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	beforeRetention := domainSnapshot()
	// Use River's real cleaner and default retention periods. Recent terminal
	// rows must survive; expired queue evidence must not delete domain history.
	retentionStarted := time.Now()
	if _, err := r.Store.Pool.Exec(ctx, `UPDATE river_job SET finalized_at=clock_timestamp()-interval '8 days' WHERE state='completed';
		INSERT INTO river_job(kind,args,queue,state,finalized_at,max_attempts)
		SELECT 'retention_fixture','{}'::jsonb,'retention_fixture',state::river_job_state,
			clock_timestamp()-age,1 FROM
			(VALUES ('completed',interval '0 days'),('cancelled',interval '0 days'),('discarded',interval '2 days'),
			('completed',interval '2 days'),('cancelled',interval '2 days'),('discarded',interval '8 days')) AS fixture(state,age)`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	await(t, 45*time.Second, func() (bool, error) {
		var remaining int
		err := r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job
			WHERE finalized_at<clock_timestamp()-interval '1 day' AND
			(state IN ('completed','cancelled') OR (state='discarded' AND finalized_at<clock_timestamp()-interval '7 days'))`).Scan(&remaining)
		return remaining == 0, err
	})
	var recent int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind='retention_fixture'").Scan(&recent); err != nil || recent != 3 {
		t.Fatalf("recent terminal jobs=%d want=3 error=%v", recent, err)
	}
	if after := domainSnapshot(); after != beforeRetention {
		t.Fatal("River cleaner changed execution history, outcomes or budgets")
	}
	t.Logf("real River retention observed after %s; recent terminal jobs=%d; domain snapshot unchanged", time.Since(retentionStarted), recent)
	// Queue uniqueness no longer remembers the expired rows; domain state must
	// fence delivery of the original completed attempts and outcomes.
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		attempts, err := store.ReadExecutionAttempts(ctx, tx, id, execution.StableID("g", id+"/root"), nil)
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if err := r.Client.InsertExecute(ctx, tx, ExecuteArgs{Attempt: attempt.AttemptID, DispatchGeneration: attempt.DispatchGeneration, RoutingVersion: RoutingVersion}); err != nil {
				return err
			}
			if err := r.Client.InsertFinalize(ctx, tx, FinalizeArgs{Attempt: attempt.AttemptID, OutcomeKey: attempt.OutcomeKey, RoutingVersion: RoutingVersion}); err != nil {
				return err
			}
		}
		state, err := store.LockExecutionRun(ctx, tx, id)
		if err != nil {
			return err
		}
		return r.Wake(ctx, tx, id, state.WakeGeneration)
	}); err != nil {
		t.Fatal(err)
	}
	await(t, 5*time.Second, queueIdle)
	var eventsAfter int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_events WHERE run_id=$1", id).Scan(&eventsAfter); err != nil || eventsAfter != eventsBefore || calls.Load() != 3 {
		t.Fatalf("retained domain state repeated work: events=%d -> %d calls=%d error=%v", eventsBefore, eventsAfter, calls.Load(), err)
	}
	if domainSnapshot() != beforeRetention {
		t.Fatal("redelivery after real queue retention changed completed domain state")
	}
}

func TestRuntimeOutcomePublicationRollsBackAndRecoversExpiredOwner(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := runtimePlan("")
	port := contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}}
	node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
	node.Outputs = map[string]contract.Port{"file": port}
	export := port
	export.Bind = &contract.Binding{From: "nodes.a.outputs.file"}
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
	artifact, err := r.Store.PutOwnedArtifact(ctx, claim.Ownership, r.Host.Artifacts, "answer.txt", "text/plain", []byte("complete artifact"), map[string]string{"runId": id, "instanceId": claim.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	outputArtifact := artifact
	outputArtifact.Path = "/worker-local/answer.txt"
	outputs := contract.Values{"file": {Artifacts: []contract.Artifact{outputArtifact}}}
	outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: plan.Digest, CompletedAt: time.Now(), Outputs: outputs, Artifacts: outcomeArtifacts(outputs)}
	key, err := r.Outcomes.Put(outcome)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("River completion failed")
	if err := r.publish(ctx, outcome, false, func(pgx.Tx) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("publication error=%v", err)
	}
	assert := func(wantState string, wantSlots, wantSuccesses int, wantPublished bool) {
		t.Helper()
		var state string
		var slots, successes int
		var published bool
		if err := r.Store.Pool.QueryRow(ctx, `SELECT n.state,s.active_attempts,a.published,
			(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'type'='node' AND document->>'message'='succeeded')
			FROM knotra_execution_nodes n JOIN knotra_execution_scopes s ON s.run_id=n.run_id AND s.id=n.run_id
			JOIN knotra_artifacts a ON a.id=$2 WHERE n.run_id=$1`, id, artifact.ID).Scan(&state, &slots, &published, &successes); err != nil {
			t.Fatal(err)
		}
		if state != wantState || slots != wantSlots || successes != wantSuccesses || published != wantPublished {
			t.Fatalf("state=%s slots=%d successes=%d published=%v", state, slots, successes, published)
		}
	}
	assert("running", 1, 0, false)
	var resultRows int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND (result IS NOT NULL OR outcome IS NOT NULL)", id).Scan(&resultRows); err != nil || resultRows != 0 {
		t.Fatalf("publication evidence escaped rollback: rows=%d error=%v", resultRows, err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := r.publish(ctx, outcome, false, func(pgx.Tx) error { return nil }); !errors.Is(err, store.ErrExecutionOwnership) {
		t.Fatalf("expired owner published: %v", err)
	}
	saved, err := r.Outcomes.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.publish(ctx, saved, true, func(pgx.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		assert("succeeded", 0, 1, true)
	}
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || len(run.Artifacts) != 1 || run.Artifacts[0].ID != artifact.ID || run.Artifacts[0].Path != "" {
		t.Fatalf("artifact export missing: %+v %v", run, err)
	}
}

func TestRuntimeBusinessRetryUsesDurableBackoffAndOriginalDeadline(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint("expire=", expire), func(t *testing.T) {
			r := newTestRuntime(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/responses" {
					t.Errorf("unexpected request %s", req.URL.Path)
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"knotra_output","arguments":"{\"answer\":\"retried\"}"}],"usage":{"input_tokens":17,"output_tokens":9}}`)
			}))
			defer server.Close()
			plan := runtimePlan(server.URL)
			node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
			node.Execution = contract.Execution{Timeout: "20s", Retry: &contract.Retry{MaxAttempts: 2, Backoff: "2s"}}
			if expire {
				// Leave room for River's one-second fetch poll before the first call.
				node.Execution.Timeout, node.Execution.Retry.Backoff = "5s", "10s"
			}
			export := node.Outputs["answer"]
			export.Bind = &contract.Binding{From: "nodes.a.outputs.answer"}
			plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"a": node}, Outputs: map[string]contract.Port{"result": export}}
			credential := "fixture-key"
			plan.Profile.Spec.Models["local"] = contract.ModelConnection{Provider: "openai", Model: "fixture", BaseURL: server.URL,
				Auth: map[string]contract.Credential{"key": {Value: &credential}}}
			ctx := context.Background()
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			id := admitRuntimeRun(t, r, plan)
			if !expire {
				await(t, 5*time.Second, func() (bool, error) {
					var waiting bool
					err := r.Store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knotra_execution_nodes WHERE run_id=$1 AND state='retry_wait')", id).Scan(&waiting)
					return waiting, err
				})
				stopCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := r.Stop(stopCtx)
				stop()
				if err != nil {
					t.Fatal(err)
				}
				var schema string
				if err := r.Store.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
					t.Fatal(err)
				}
				replacement := &Runtime{Store: r.Store, Host: r.Host, Outcomes: r.Outcomes, WorkerID: uuid.NewString(), HostID: r.HostID}
				replacement.Client = newTestClient(t, testDatabase{pool: r.Store.Pool, schema: schema}, r.Store.EngineID, replacement.HostID, time.Minute, replacement.Handlers())
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := replacement.Stop(ctx); err != nil {
						t.Error(err)
					}
				})
				if err := replacement.Start(ctx); err != nil {
					t.Fatal(err)
				}
				r = replacement
			}
			await(t, 15*time.Second, func() (bool, error) {
				status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
				return protocol.Terminal(status), err
			})
			run, err := r.Store.Run(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantCalls := "succeeded", 2
			if expire {
				wantStatus, wantCalls = "failed", 1
				if len(run.Diagnostics) == 0 || run.Diagnostics[len(run.Diagnostics)-1].Code != "DEADLINE_EXCEEDED" {
					t.Fatalf("deadline failure=%+v", run.Diagnostics)
				}
			}
			var used, attempts int
			var originalDeadline bool
			if err := r.Store.Pool.QueryRow(ctx, `SELECT
				(SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$1 AND kind='model'),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
				(SELECT bool_and((execution_request->>'deadline')::timestamptz=deadline) FROM knotra_execution_nodes WHERE run_id=$1)`, id).Scan(&used, &attempts, &originalDeadline); err != nil {
				t.Fatal(err)
			}
			if run.Status != wantStatus || int(calls.Load()) != wantCalls || used != wantCalls || attempts != wantCalls || !originalDeadline {
				t.Fatalf("status=%s calls=%d used=%d attempts=%d originalDeadline=%v", run.Status, calls.Load(), used, attempts, originalDeadline)
			}
			var backoff, admittedAt time.Time
			if !expire {
				if err := r.Store.Pool.QueryRow(ctx, `SELECT t.due_at,o.admitted_at FROM knotra_execution_timers t
					JOIN knotra_operations o ON o.run_id=t.run_id AND o.instance_id=t.instance_id AND o.attempt_number=2
					WHERE t.run_id=$1 AND t.kind='retry'`, id).Scan(&backoff, &admittedAt); err != nil || admittedAt.Before(backoff) {
					t.Fatalf("retry started before persisted backoff: %v < %v, error=%v", admittedAt, backoff, err)
				}
			}
		})
	}
}
