package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/api"
	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func compositePort(schema, from string) contract.Port {
	port := contract.Port{Schema: json.RawMessage(schema)}
	if from != "" {
		port.Bind = &contract.Binding{From: from}
	}
	return port
}

func foreachPlan(endpoint string, human bool) contract.Plan {
	plan := runtimePlan(endpoint)
	integer, array := `{"type":"integer"}`, `{"type":"array","items":{"type":"integer"}}`
	work := plan.Pipelines[plan.Root].Spec.Nodes["a"]
	work.Inputs = map[string]contract.Port{"item": compositePort(integer, "inputs.item")}
	work.Outputs = map[string]contract.Port{"value": compositePort(integer, "")}
	if human {
		work.Type, work.LLM, work.Human = "human", nil, &contract.HumanNode{Prompt: contract.TextSource{Text: "Echo the item."}}
	}
	body := contract.Graph{Inputs: map[string]contract.Port{"item": compositePort(integer, "")},
		Nodes: map[string]contract.Node{"work": work}, Outputs: map[string]contract.Port{"result": compositePort(integer, "nodes.work.outputs.value")}}
	items := compositePort(array, "")
	items.Bind = &contract.Binding{Value: json.RawMessage(`[3,1,2]`)}
	node := contract.Node{Type: "foreach", Inputs: map[string]contract.Port{"items": items},
		Outputs: map[string]contract.Port{"result": compositePort(array, "")},
		Foreach: &contract.ForeachNode{Over: "items", Concurrency: 2, With: map[string]contract.Binding{"item": {From: "iteration.item"}}, Body: body}}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"map": node}, Outputs: map[string]contract.Port{"result": compositePort(array, "nodes.map.outputs.result")}}
	return plan
}

// A human wait occupies a foreach body position, but never an execution slot.
// Replace the process incarnation while bodies wait and answer out of order.
func TestRuntimeForeachHumanWaitResumesWithoutRematerializingChildren(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := admitRuntimeRun(t, r, foreachPlan("", true))
	assert := func(children, open int) {
		t.Helper()
		await(t, 5*time.Second, func() (bool, error) {
			var gotChildren, gotOpen, slots, attempts int
			err := r.Store.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id IS NOT NULL),
				(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
				(SELECT active_attempts FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1),
				(SELECT count(*) FROM knotra_execution_attempts WHERE run_id=$1)`, id).Scan(&gotChildren, &gotOpen, &slots, &attempts)
			if slots != 0 || attempts != 0 {
				return false, fmt.Errorf("human bodies acquired slots=%d attempts=%d", slots, attempts)
			}
			return gotChildren == children && gotOpen == open, err
		})
	}
	assert(2, 2)
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := r.Stop(stop)
	cancel()
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
	assert(2, 2)
	srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
	parent := execution.StableID("n", id+"/root/map")
	for _, item := range []struct{ index, value int }{{1, 1}, {0, 3}, {2, 2}} {
		instance := execution.StableID("n", fmt.Sprintf("%s/item/%d/work", parent, item.index))
		request := execution.StableID("h", instance+"/human")
		body := fmt.Sprintf(`{"outputs":{"value":%d}}`, item.value)
		path := "/v1/requests/" + request + "/response"
		w := runtimeCommand(t, srv.Handler(), path, request, body)
		if w.Code != 200 {
			t.Fatalf("human %d: %d %s", item.index, w.Code, w.Body.String())
		}
		duplicate := runtimeCommand(t, srv.Handler(), path, request, body)
		if duplicate.Code != w.Code || duplicate.Body.String() != w.Body.String() {
			t.Fatal("accepted child response receipt changed")
		}
		if item.index == 1 {
			assert(3, 2)
		}
	}
	await(t, 5*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `[3,1,2]` {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	assert(3, 0)
	var materialized, successes int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT materialized_instances,
		(SELECT count(*) FROM knotra_events WHERE run_id=$1 AND document->>'message'='succeeded' AND document->>'type'='node')
		FROM knotra_execution_scopes WHERE run_id=$1 AND id=$1`, id).Scan(&materialized, &successes); err != nil || materialized != 4 || successes != 4 {
		t.Fatalf("materialized=%d successes=%d error=%v", materialized, successes, err)
	}
}

func TestRuntimeNestedPipelineLoopForeachUsesAncestorBudgets(t *testing.T) {
	for _, cap := range []int{6, 3} {
		t.Run(fmt.Sprint("childBudget=", cap), func(t *testing.T) {
			r := newTestRuntime(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var input struct {
					Values map[string]int `json:"values"`
				}
				for _, message := range body.Messages {
					if text, found := strings.CutPrefix(message.Content, "Knotra input context (data):\n"); found {
						if err := json.Unmarshal([]byte(text), &input); err != nil {
							t.Error(err)
						}
					}
				}
				item, found := input.Values["item"]
				if !found {
					t.Error("provider did not receive the complete child input")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				calls.Add(1)
				content, _ := json.Marshal(map[string]int{"value": item})
				_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "done": true, "done_reason": "stop"})
			}))
			defer server.Close()
			plan := foreachPlan(server.URL, false)
			array, integer, text := `{"type":"array","items":{"type":"integer"}}`, `{"type":"integer"}`, `{"type":"string"}`
			loop := contract.Node{Type: "loop", Loop: &contract.LoopNode{MaxIterations: 2, Until: "iteration.index == 1", Body: plan.Pipelines[plan.Root].Spec.Graph},
				Outputs: map[string]contract.Port{"result": compositePort(array, ""), "iterations": compositePort(integer, ""), "termination": compositePort(text, "")}}
			child := *plan.Pipelines[plan.Root]
			child.Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"repeat": loop}, Outputs: map[string]contract.Port{"result": compositePort(array, "nodes.repeat.outputs.result"), "iterations": compositePort(integer, "nodes.repeat.outputs.iterations")}}
			child.Spec.Limits = contract.Limits{MaxConcurrentNodes: 1, MaxModelCalls: cap, MaxNodeInstances: 20, Timeout: "40s"}
			plan.Pipelines["child.yaml"] = &child
			node := contract.Node{Type: "pipeline", Pipeline: &contract.PipelineNode{File: "child.yaml", Permissions: contract.Permissions{Models: []string{"model"}}},
				Outputs: map[string]contract.Port{"result": compositePort(array, ""), "iterations": compositePort(integer, "")}}
			plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"child": node}, Outputs: map[string]contract.Port{"result": compositePort(array, "nodes.child.outputs.result"), "iterations": compositePort(integer, "nodes.child.outputs.iterations")}}
			ctx := context.Background()
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			id := admitRuntimeRun(t, r, plan)
			await(t, 15*time.Second, func() (bool, error) {
				status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
				return protocol.Terminal(status), err
			})
			run, err := r.Store.Run(ctx, id)
			wantStatus := "succeeded"
			if cap == 3 {
				wantStatus = "failed"
			}
			if err != nil || run.Status != wantStatus || calls.Load() != int32(cap) {
				t.Fatalf("run=%+v calls=%d error=%v", run, calls.Load(), err)
			}
			if cap == 6 && (string(run.Outputs["result"]) != `[3,1,2]` || string(run.Outputs["iterations"]) != "2") {
				t.Fatalf("nested exports changed: %+v", run.Outputs)
			}
			if cap == 3 && (len(run.Diagnostics) == 0 || run.Diagnostics[len(run.Diagnostics)-1].Code != "BUDGET_EXCEEDED") {
				t.Fatalf("child budget failure=%+v", run.Diagnostics)
			}
			parent := execution.StableID("n", id+"/root/child")
			var budgets []int
			rows, err := r.Store.Pool.Query(ctx, "SELECT used FROM knotra_budgets WHERE run_id=$1 AND kind='model' ORDER BY scope", id)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var used int
				if err := rows.Scan(&used); err != nil {
					t.Fatal(err)
				}
				budgets = append(budgets, used)
			}
			rows.Close()
			if rows.Err() != nil || !reflect.DeepEqual(budgets, []int{cap, cap}) {
				t.Fatalf("ancestor budgets=%v error=%v", budgets, rows.Err())
			}
			if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
				root, err := store.ReadExecutionGraph(ctx, tx, id, execution.StableID("g", id+"/root"))
				if err != nil {
					return err
				}
				children, err := store.ReadChildExecutionGraphs(ctx, tx, id, root.ID, nil)
				if err != nil {
					return err
				}
				if len(children) != 1 || len(children[0].Scopes) != 2 || children[0].Scopes[1].ID != parent || children[0].Permissions == nil || !reflect.DeepEqual(children[0].Permissions.Models, []string{"model"}) {
					return fmt.Errorf("child scopes/permissions=%+v", children)
				}
				scopes, err := store.ReadExecutionScopes(ctx, tx, children[0])
				if err != nil {
					return err
				}
				for _, scope := range scopes {
					if scope.ActiveAttempts != 0 {
						return fmt.Errorf("leaked ancestor slots: %+v", scope)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeLoopReadsOnlyCurrentChildAndRetainsIterationEvidence(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := runtimePlan("")
	text, integer := `{"type":"string"}`, `{"type":"integer"}`
	body := contract.Graph{
		Nodes:   map[string]contract.Node{"gate": {Type: "switch", Switch: &contract.SwitchNode{Default: "done"}, Outputs: map[string]contract.Port{"route": compositePort(text, "")}}},
		Outputs: map[string]contract.Port{"branch": compositePort(text, "nodes.gate.outputs.route")},
	}
	loop := contract.Node{Type: "loop", Loop: &contract.LoopNode{MaxIterations: 70, Until: "iteration.index == 69", Body: body},
		Outputs: map[string]contract.Port{"branch": compositePort(text, ""), "iterations": compositePort(integer, ""), "termination": compositePort(text, "")}}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"repeat": loop},
		Outputs: map[string]contract.Port{"iterations": compositePort(integer, "nodes.repeat.outputs.iterations")}}
	id := admitRuntimeRun(t, r, plan)
	// Drive production advance transactions without waiting on native queue polls.
	// This regression concerns loaded history, not notification latency.
	for range 1000 {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") }); err != nil {
			t.Fatal(err)
		}
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		if err != nil {
			t.Fatal(err)
		}
		if protocol.Terminal(status) {
			break
		}
	}
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["iterations"]) != "70" {
		t.Fatalf("loop status=%s iterations=%s diagnostics=%+v error=%v", run.Status, run.Outputs["iterations"], run.Diagnostics, err)
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		children, err := store.ReadChildExecutionGraphs(ctx, tx, id, execution.StableID("g", id+"/root"), nil)
		if err != nil {
			return err
		}
		if len(children) != 1 || children[0].IterationIndex == nil || *children[0].IterationIndex != 69 || string(children[0].Outputs["branch"].JSON) != `"done"` {
			return fmt.Errorf("scheduler loaded historical iterations: %+v", children)
		}
		var graphs, successes int
		if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE state='succeeded')
			FROM knotra_execution_graphs WHERE run_id=$1 AND parent_instance_id=$2`, id, execution.StableID("n", id+"/root/repeat")).Scan(&graphs, &successes); err != nil {
			return err
		}
		if graphs != 70 || successes != 70 {
			return fmt.Errorf("completed iteration evidence changed: graphs=%d succeeded=%d", graphs, successes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeForeachCollectionPagesRollbackAndResumeFromStore(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := foreachPlan("", false)
	plan.Profile.Spec.Limits.MaxNodeInstances = 200
	values := make([]int, 130)
	for i := range values {
		values[i] = len(values) - i
	}
	expected, _ := json.Marshal(values)
	node := plan.Pipelines[plan.Root].Spec.Nodes["map"]
	input := node.Inputs["items"]
	input.Bind.Value = expected
	node.Inputs["items"] = input
	node.Foreach.Concurrency = len(values)
	node.Foreach.Body.Nodes = map[string]contract.Node{"gate": {Type: "switch", Switch: &contract.SwitchNode{Default: "done"},
		Outputs: map[string]contract.Port{"route": compositePort(`{"type":"string"}`, "")}}}
	node.Foreach.Body.Outputs["result"] = compositePort(`{"type":"integer"}`, "inputs.item")
	plan.Pipelines[plan.Root].Spec.Nodes["map"] = node
	id := admitRuntimeRun(t, r, plan)
	rootID := execution.StableID("g", id+"/root")
	var control execution.ControlRecord
	for range 1024 {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
				return err
			}
			controls, err := store.ReadExecutionControls(ctx, tx, id, rootID, nil)
			if err != nil || len(controls) == 0 {
				return err
			}
			control = controls[0]
			if control.ActiveChildren+control.SucceededChildren+control.FailedChildren != control.NextPosition {
				return fmt.Errorf("child counters drifted: %+v", control)
			}
			if control.ActiveChildren > 0 {
				children, err := store.ReadChildExecutionGraphs(ctx, tx, id, rootID, nil)
				if err != nil {
					return err
				}
				if len(children) != 0 {
					return fmt.Errorf("pending foreach loaded %d complete child records", len(children))
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if control.SucceededChildren == len(values) {
			break
		}
	}
	if control.SucceededChildren != len(values) || control.CollectionPosition != 0 {
		t.Fatalf("unexpected collection boundary: %+v", control)
	}
	advancePage := func(runtime *Runtime, cursor int) error {
		return pgx.BeginFunc(ctx, runtime.Store.Pool, func(tx pgx.Tx) error {
			children, err := store.ReadChildExecutionGraphs(ctx, tx, id, rootID, nil)
			if err != nil {
				return err
			}
			if len(children) != min(64, len(values)-cursor) || children[0].IterationIndex == nil || *children[0].IterationIndex != cursor {
				return fmt.Errorf("collection page cursor=%d records=%d", cursor, len(children))
			}
			duplicate := children[0]
			duplicate.Revision++
			duplicate.Outputs = contract.Values{"result": {JSON: json.RawMessage(`-1`)}}
			if err := store.SaveExecutionGraph(ctx, tx, duplicate); !errors.Is(err, store.ErrConflict) {
				return errors.Join(errors.New("completed child was mutable or counted again"), err)
			}
			changes, err := runtime.advanceGraphTx(ctx, tx, plan, id, rootID, "", 1)
			if err == nil && (changes.Transitions != 1 || !changes.More) {
				err = fmt.Errorf("collection did not consume one bounded transition: %+v", changes)
			}
			return err
		})
	}
	injected := errors.New("crash before collection transaction commit")
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 1); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("collection rollback=%v", err)
	}
	if err := advancePage(r, 0); err != nil {
		t.Fatal(err)
	}
	// Reopen the schema with a new store/runtime; cursor recovery uses no Go heap.
	reopened, err := store.Open(ctx, r.Store.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := &Runtime{Store: reopened, Client: r.Client, Host: r.Host}
	for _, cursor := range []int{64, 128} {
		if err := advancePage(replacement, cursor); err != nil {
			t.Fatal(err)
		}
	}
	if err := pgx.BeginFunc(ctx, reopened.Pool, func(tx pgx.Tx) error { return replacement.advanceTx(ctx, tx, plan, id, "") }); err != nil {
		t.Fatal(err)
	}
	run, err := reopened.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != string(expected) {
		t.Fatalf("ordered collection status=%s result=%s error=%v", run.Status, run.Outputs["result"], err)
	}
}

func TestRuntimeForeachCollectsDockerArtifactsInOrder(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_HELPER") == "" {
		t.Skip("set KNOTRA_TEST_HELPER for real foreach artifact collection")
	}
	for _, count := range []int{0, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := newTestRuntime(t)
			ctx := t.Context()
			plan := resourceCodePlan(t, r, 0)
			code := plan.Pipelines[plan.Root].Spec.Nodes["a"]
			code.Inputs = map[string]contract.Port{"item": compositePort(`{"type":"integer"}`, "inputs.item")}
			code.Outputs = map[string]contract.Port{"file": {Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}}, Collect: &contract.Collect{Path: "report.txt", MediaType: "text/plain"}}}
			code.Code.Command = []string{"python", "-c", `import json,os;v=json.load(open(os.environ["KNOTRA_INPUT_JSON"]))["values"]["item"];open("report.txt","w").write(str(v));json.dump({},open(os.environ["KNOTRA_OUTPUT_JSON"],"w"))`}
			items := []int{3, 1, 2}[:count]
			raw, _ := json.Marshal(items)
			bodyPort := code.Outputs["file"]
			bodyPort.Collect, bodyPort.Bind = nil, &contract.Binding{From: "nodes.write.outputs.file"}
			collection := contract.Port{Artifact: &contract.ArtifactPort{MediaTypes: []string{"text/plain"}, Collection: true}}
			node := contract.Node{Type: "foreach", Inputs: map[string]contract.Port{"items": {Schema: json.RawMessage(`{"type":"array","items":{"type":"integer"}}`), Bind: &contract.Binding{Value: raw}}}, Outputs: map[string]contract.Port{"files": collection},
				Foreach: &contract.ForeachNode{Over: "items", Concurrency: 2, With: map[string]contract.Binding{"item": {From: "iteration.item"}}, Body: contract.Graph{
					Inputs: map[string]contract.Port{"item": compositePort(`{"type":"integer"}`, "")}, Nodes: map[string]contract.Node{"write": code}, Outputs: map[string]contract.Port{"files": bodyPort}}}}
			export := collection
			export.Bind = &contract.Binding{From: "nodes.map.outputs.files"}
			plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"map": node}, Outputs: map[string]contract.Port{"files": export}}
			id := admitRuntimeRun(t, r, plan)
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			await(t, 20*time.Second, func() (bool, error) {
				status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
				return protocol.Terminal(status), err
			})
			run, err := r.Store.Run(ctx, id)
			if err != nil || run.Status != "succeeded" || len(run.Artifacts) != count {
				t.Fatalf("artifact collection status=%s artifacts=%d diagnostics=%+v error=%v", run.Status, len(run.Artifacts), run.Diagnostics, err)
			}
			for i, artifact := range run.Artifacts {
				data, err := r.Host.Artifacts.Get(ctx, artifact.ID)
				if err != nil || string(data) != fmt.Sprint(items[i]) || artifact.Path != "" {
					t.Fatalf("artifact %d bytes=%s path=%s error=%v", i, data, artifact.Path, err)
				}
			}
		})
	}
}

func TestRuntimeCompositeChildCreationRollsBackWithContinuation(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := foreachPlan("", true)
	id := admitRuntimeRun(t, r, plan)
	advance := func() error {
		return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") })
	}
	for range 2 {
		if err := advance(); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(position, graphs int) {
		t.Helper()
		var gotPosition, gotGraphs, timers int
		if err := r.Store.Pool.QueryRow(ctx, `SELECT c.next_position,
			(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1),
			(SELECT count(*) FROM knotra_execution_timers WHERE run_id=$1 AND id LIKE 'graph/%')
			FROM knotra_execution_controls c WHERE run_id=$1`, id).Scan(&gotPosition, &gotGraphs, &timers); err != nil {
			t.Fatal(err)
		}
		if gotPosition != position || gotGraphs != graphs || timers != position {
			t.Fatalf("position=%d graphs=%d childTimers=%d", gotPosition, gotGraphs, timers)
		}
	}
	assert(0, 1)
	var wakes int64
	var jobs int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation,(SELECT count(*) FROM river_job) FROM knotra_runs WHERE id=$1", id).Scan(&wakes, &jobs); err != nil {
		t.Fatal(err)
	}
	injected := fmt.Errorf("crash before child transaction commit")
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("rollback error=%v", err)
	}
	assert(0, 1)
	var wakesAfter int64
	var jobsAfter int
	if err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation,(SELECT count(*) FROM river_job) FROM knotra_runs WHERE id=$1", id).Scan(&wakesAfter, &jobsAfter); err != nil || wakesAfter != wakes || jobsAfter != jobs {
		t.Fatalf("continuation escaped rollback: wakes=%d/%d jobs=%d/%d error=%v", wakesAfter, wakes, jobsAfter, jobs, err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	assert(1, 2)
	var frozen bool
	if err := r.Store.Pool.QueryRow(ctx, `SELECT g.deadline=n.deadline AND c.elements->0->>'json'='3'
		FROM knotra_execution_controls c JOIN knotra_execution_nodes n ON n.run_id=c.run_id AND n.id=c.instance_id
		JOIN knotra_execution_graphs g ON g.run_id=c.run_id AND g.parent_instance_id=c.instance_id
		WHERE c.run_id=$1`, id).Scan(&frozen); err != nil || !frozen {
		t.Fatalf("child deadline/elements changed: %v %v", frozen, err)
	}
}

func TestRuntimeGraphScanFinishesLargeHumanWaitAndCancelsEveryChild(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	plan := foreachPlan("", true)
	document := plan.Pipelines[plan.Root]
	node := document.Spec.Nodes["map"]
	items := make([]int, 70)
	for i := range items {
		items[i] = i
	}
	port := node.Inputs["items"]
	port.Bind.Value, _ = json.Marshal(items)
	node.Inputs["items"], node.Foreach.Concurrency = port, len(items)
	document.Spec.Nodes["map"] = node
	id := admitRuntimeRun(t, r, plan)
	advance := func() {
		t.Helper()
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			applied, err := store.ApplyWake(ctx, tx, id, run.WakeGeneration)
			if err != nil || !applied {
				return err
			}
			return r.advanceTx(ctx, tx, plan, id, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 150 {
		advance()
		var open int
		if err := r.Store.Pool.QueryRow(ctx, "SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'", id).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == len(items) {
			break
		}
	}
	var open, graphs int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
		(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1)`, id).Scan(&open, &graphs); err != nil || open != len(items) || graphs != len(items)+1 {
		t.Fatalf("waiting bodies open=%d graphs=%d error=%v", open, graphs, err)
	}
	// Finish the bounded scan and verify that waiting alone creates no more
	// continuations. A queue poll must not spin forever on 70 human waits.
	for range 2 {
		advance()
	}
	var before int64
	err := r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&before)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		advance()
	}
	var after int64
	err = r.Store.Pool.QueryRow(ctx, "SELECT wake_generation FROM knotra_runs WHERE id=$1", id).Scan(&after)
	if err != nil || before != after {
		t.Fatalf("waiting scan generated wakeups: %d -> %d, %v", before, after, err)
	}
	parent := execution.StableID("n", id+"/root/map")
	instance := execution.StableID("n", fmt.Sprintf("%s/item/69/work", parent))
	request := execution.StableID("h", instance+"/human")
	// Force the ordinary scan cursor beyond this request. Accepted commands
	// still consume their node transition inside their receipt transaction.
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET graph_cursor='zzzz' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Store: r.Store, Artifacts: r.Host.Artifacts, Wake: r.CommandWake}
	if w := runtimeCommand(t, srv.Handler(), "/v1/requests/"+request+"/response", "accepted-last", `{"outputs":{"value":69}}`); w.Code != 200 {
		t.Fatalf("answer=%d %s", w.Code, w.Body.String())
	}
	var state string
	if err := r.Store.Pool.QueryRow(ctx, "SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", id, instance).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("accepted node=%s error=%v", state, err)
	}
	if w := runtimeCommand(t, srv.Handler(), "/v1/runs/"+id+"/cancel", "cancel", `{}`); w.Code != 202 {
		t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
	}
	for range 5 {
		advance()
	}
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	var active int
	if err := r.Store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM knotra_execution_graphs WHERE run_id=$1 AND state IN ('pending','running'))+
		(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open')+
		(SELECT sum(active_attempts) FROM knotra_execution_scopes WHERE run_id=$1)`, id).Scan(&active); err != nil || active != 0 {
		t.Fatalf("unfinished cancellation records=%d error=%v", active, err)
	}
}
