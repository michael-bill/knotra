package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func runtimeCommand(t *testing.T, handler http.Handler, route, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, route, strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestRuntimeHumanCommandIsAtomicAndFirstValidAnswerWins(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := runtimePlan("")
	port := contract.Port{Schema: json.RawMessage(`{"type":"boolean"}`)}
	human := contract.Node{Type: "human", Human: &contract.HumanNode{Prompt: contract.TextSource{Text: "Approve?"}},
		Outputs: map[string]contract.Port{"approved": port}, Execution: contract.Execution{Timeout: "30s"}}
	export := port
	export.Bind = &contract.Binding{From: "nodes.review.outputs.approved"}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"review": human, "wait": human}, Outputs: map[string]contract.Port{"result": export}}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, plan)
	instance := execution.StableID("n", id+"/root/review")
	requestID := execution.StableID("h", instance+"/human")
	await(t, 5*time.Second, func() (bool, error) {
		var open int
		err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'", id).Scan(&open)
		return open == 2, err
	})
	srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
	path := "/v1/requests/" + requestID + "/response"
	if w := runtimeCommand(t, srv.Handler(), path, "invalid", `{"outputs":{"approved":"wrong"}}`); w.Code != 422 {
		t.Fatalf("invalid response=%d %s", w.Code, w.Body.String())
	}
	assertState := func(wantNode, wantRequest string, wantOpen int) {
		t.Helper()
		var node, request string
		var open, slots, attempts, outbox int
		if err := r.Store.Pool.QueryRow(ctx, `SELECT n.state,q.status,
			(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
			(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
			(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1),
			(SELECT count(*) FROM knotra_outbox WHERE run_id=$1)
			FROM knotra_execution_nodes n JOIN knotra_requests q ON q.id=$3 WHERE n.run_id=$1 AND n.id=$2`,
			id, instance, requestID).Scan(&node, &request, &open, &slots, &attempts, &outbox); err != nil {
			t.Fatal(err)
		}
		if node != wantNode || request != wantRequest || open != wantOpen || slots != 0 || attempts != 0 || outbox != 0 {
			t.Fatalf("node=%s request=%s open=%d slots=%d attempts=%d temporalCommands=%d", node, request, open, slots, attempts, outbox)
		}
	}
	assertState("waiting_human", "open", 2)
	injected := errors.New("enqueue failed after scheduler changes")
	fail := func(ctx context.Context, tx pgx.Tx, id string, generation int64) error {
		if err := r.CommandWake(ctx, tx, id, generation); err != nil {
			return err
		}
		return injected
	}
	// The command savepoint must protect the answer and scheduler changes even
	// if a caller mistakenly commits its outer transaction after enqueue error.
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := store.Respond(ctx, tx, requestID, "rolled-back", contract.Values{"approved": {JSON: json.RawMessage("true")}}, fail); !errors.Is(err, injected) {
			return fmt.Errorf("unexpected command error: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertState("waiting_human", "open", 2)
	w := runtimeCommand(t, srv.Handler(), path, "accepted", `{"outputs":{"approved":true}}`)
	if w.Code != 200 {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	assertState("succeeded", "accepted", 1)
	requestsResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(requestsResponse, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/requests", nil))
	var requestsPage protocol.Page[protocol.HumanRequest]
	if err := json.Unmarshal(requestsResponse.Body.Bytes(), &requestsPage); err != nil {
		t.Fatal(err)
	}
	var answered bool
	for _, request := range requestsPage.Items {
		if request.ID == requestID {
			answered = request.Status == "answered"
		}
	}
	if requestsResponse.Code != http.StatusOK || !answered {
		t.Fatalf("consumed answer broke desktop protocol: %d %s", requestsResponse.Code, requestsResponse.Body.String())
	}
	duplicate := runtimeCommand(t, srv.Handler(), path, "accepted", `{"outputs":{"approved":true}}`)
	if duplicate.Code != w.Code || duplicate.Body.String() != w.Body.String() {
		t.Fatalf("receipt replay changed: %d %s", duplicate.Code, duplicate.Body.String())
	}
	if w := runtimeCommand(t, srv.Handler(), path, "second", `{"outputs":{"approved":false}}`); w.Code != 409 {
		t.Fatalf("second answer replaced first: %d %s", w.Code, w.Body.String())
	}
	if w := runtimeCommand(t, srv.Handler(), "/v1/runs/"+id+"/cancel", "cancel", `{}`); w.Code != 202 {
		t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
	}
	assertState("succeeded", "accepted", 0)
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("cancelled run=%+v error=%v", run, err)
	}
	var value string
	var successes int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT outputs->'approved'->>'json',
		(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'instanceId'=$2 AND document->>'message'='succeeded')
		FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2`, id, instance).Scan(&value, &successes); err != nil || value != "true" || successes != 1 {
		t.Fatalf("terminal answer changed: value=%s successes=%d error=%v", value, successes, err)
	}
}

func TestRuntimeResolutionClearsPauseOnlyAfterEveryDecision(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	// A received response becomes uncertain if its journal commit fails.
	// Model transport/status errors alone permit a read retry and do not pause.
	if _, err := r.Store.Pool.Exec(ctx, `CREATE FUNCTION lose_model_response() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.completed AND EXISTS(SELECT 1 FROM knotra_execution_nodes n
				WHERE n.run_id=NEW.run_id AND n.id=NEW.instance_id AND n.node_id IN ('a','b')) THEN
				RAISE EXCEPTION 'test lost model response';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER lose_model_response BEFORE UPDATE ON knotra_operations
		FOR EACH ROW EXECUTE FUNCTION lose_model_response()`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	bothAdmitted := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			if n == 2 {
				close(bothAdmitted)
			}
			select {
			case <-bothAdmitted:
			case <-req.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"{\"answer\":\"third\"}"},"done":true,"done_reason":"stop"}`)
	}))
	defer provider.Close()
	plan := runtimePlan(provider.URL)
	plan.Profile.Spec.Limits.MaxConcurrentNodes = 2
	node := plan.Pipelines[plan.Root].Spec.Nodes["a"]
	plan.Pipelines[plan.Root].Spec.Nodes = map[string]contract.Node{"a": node, "b": node, "c": node}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, plan)
	t.Cleanup(func() {
		if t.Failed() {
			var state []byte
			_ = r.Store.Pool.QueryRow(ctx, `SELECT jsonb_build_object('requests',(SELECT jsonb_agg(to_jsonb(q)) FROM knotra_requests q WHERE run_id=$1),'nodes',(SELECT jsonb_agg(to_jsonb(n)) FROM knotra_execution_nodes n WHERE run_id=$1),'attempts',(SELECT jsonb_agg(to_jsonb(a)) FROM knotra_execution_attempts a WHERE run_id=$1))`, id).Scan(&state)
			t.Logf("runtime state: %s", state)
		}
	})
	await(t, 10*time.Second, func() (bool, error) {
		var unresolved, pending int
		err := r.Store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND kind='resolution' AND status='open'),
			(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1 AND state='ready' AND dispatch_pending)`, id).Scan(&unresolved, &pending)
		return unresolved == 2 && pending == 1, err
	})
	var instances []string
	rows, err := r.Store.Pool.Query(ctx, "SELECT id FROM knotra_execution_nodes WHERE run_id=$1 AND state='waiting_resolution' ORDER BY id", id)
	if err == nil {
		instances, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || len(instances) != 2 {
		t.Fatalf("unresolved nodes=%v error=%v", instances, err)
	}
	handler := (&api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}).Handler()
	path := func(instance string) string { return "/v1/runs/" + id + "/instances/" + instance + "/resolve" }
	if w := runtimeCommand(t, handler, path(instances[0]), "invalid-resolution", `{"outcome":"succeeded","evidence":"record","outputs":{"answer":42}}`); w.Code != 422 {
		t.Fatalf("invalid resolution=%d %s", w.Code, w.Body.String())
	}
	first := runtimeCommand(t, handler, path(instances[0]), "resolve-first", `{"outcome":"succeeded","evidence":"external record 1","outputs":{"answer":"confirmed"}}`)
	if first.Code != 202 {
		t.Fatalf("first resolution=%d %s", first.Code, first.Body.String())
	}
	run, err := r.Store.Run(ctx, id)
	var paused bool
	if err == nil {
		err = r.Store.Pool.QueryRow(ctx, "SELECT admission_paused FROM knotra_runs WHERE id=$1", id).Scan(&paused)
	}
	if err != nil || run.Status != "waiting_resolution" || !paused || calls.Load() != 2 {
		t.Fatalf("first decision resumed root: status=%s paused=%v calls=%d error=%v", run.Status, paused, calls.Load(), err)
	}
	if w := runtimeCommand(t, handler, path(instances[0]), "resolve-closed", `{"outcome":"succeeded","evidence":"different","outputs":{"answer":"changed"}}`); w.Code != 409 {
		t.Fatalf("closed resolution on a paused run=%d %s", w.Code, w.Body.String())
	}
	if w := runtimeCommand(t, handler, path("missing-instance"), "resolve-missing", `{"outcome":"failed","evidence":"record"}`); w.Code != 404 {
		t.Fatalf("missing resolution=%d %s", w.Code, w.Body.String())
	}
	if w := runtimeCommand(t, handler, path(instances[1]), "resolve-last", `{"outcome":"succeeded","evidence":"external record 2","outputs":{"answer":"confirmed"}}`); w.Code != 202 {
		t.Fatalf("last resolution=%d %s", w.Code, w.Body.String())
	}
	await(t, 10*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err = r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || calls.Load() != 3 {
		t.Fatalf("resolved run=%+v calls=%d error=%v", run, calls.Load(), err)
	}
	if w := runtimeCommand(t, handler, path(instances[0]), "resolve-again", `{"outcome":"succeeded","evidence":"different","outputs":{"answer":"changed"}}`); w.Code != 409 {
		t.Fatalf("operator decision was replaced: %d %s", w.Code, w.Body.String())
	}
	duplicate := runtimeCommand(t, handler, path(instances[0]), "resolve-first", `{"outcome":"succeeded","evidence":"external record 1","outputs":{"answer":"confirmed"}}`)
	if duplicate.Code != first.Code || duplicate.Body.String() != first.Body.String() {
		t.Fatalf("resolution receipt changed: %d %s", duplicate.Code, duplicate.Body.String())
	}
}
