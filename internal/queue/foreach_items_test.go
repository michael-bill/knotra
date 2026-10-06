package queue

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
)

func TestRuntimeForeachReadsOneElementAndOnlyReferencedInputs(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := runtimePlan("")
	plan.Profile.Spec.Limits.Timeout = "5m"
	text, array := `{"type":"string"}`, `{"type":"array","items":{"type":"string"}}`
	body := contract.Graph{Inputs: map[string]contract.Port{"item": compositePort(text, ""), "label": compositePort(text, "")},
		Outputs: map[string]contract.Port{"result": {Schema: json.RawMessage(text), Bind: &contract.Binding{Expr: "inputs.label + inputs.item"}}}}
	foreach := contract.Node{Type: "foreach", When: "inputs.enabled", Execution: contract.Execution{Timeout: "5m"},
		Inputs:  map[string]contract.Port{"items": compositePort(array, "inputs.items"), "unused": compositePort(text, "inputs.unused"), "label": compositePort(text, "inputs.label")},
		Outputs: map[string]contract.Port{"result": compositePort(array, "")},
		Foreach: &contract.ForeachNode{Over: "items", Concurrency: 8, Body: body,
			With: map[string]contract.Binding{"item": {From: "iteration.item"}, "label": {From: "args.label"}}}}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{
		Inputs: map[string]contract.Port{"items": compositePort(array, ""), "unused": compositePort(text, ""), "label": compositePort(text, ""), "enabled": compositePort(`{"type":"boolean"}`, "")},
		Nodes:  map[string]contract.Node{"map": foreach}, Outputs: map[string]contract.Port{"result": compositePort(array, "nodes.map.outputs.result")}}
	const count = 257
	items, expected := make([]string, count), make([]string, count)
	for i := range items {
		items[i] = fmt.Sprintf("%04d/", i) + strings.Repeat("payload-", 2048)
		expected[i] = "prefix/" + items[i]
	}
	input, _ := json.Marshal(items)
	unused, _ := json.Marshal(strings.Repeat("unrelated", 100000))
	id := admitRuntimeRun(t, r, plan, contract.Values{"items": {JSON: input}, "unused": {JSON: unused}, "label": {JSON: json.RawMessage(`"prefix/"`)}, "enabled": {JSON: json.RawMessage(`true`)}})
	rootID, parentID := execution.StableID("g", id+"/root"), execution.StableID("n", id+"/root/map")
	for range 2 {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			_, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 64)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertSnapshot := func(runtime *Runtime, position int) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, runtime.Store.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			root, err := store.ReadExecutionGraphMetadata(ctx, tx, id, rootID)
			if err != nil {
				return err
			}
			if root.Inputs != nil {
				return errors.New("coordination reloaded the complete graph input array")
			}
			nodes, err := store.ReadSchedulableNodes(ctx, tx, run, root, "", 64)
			if err != nil || len(nodes) != 1 || !nodes[0].InputsOmitted || nodes[0].Inputs != nil || nodes[0].Request != nil {
				return errors.Join(errors.New("foreach snapshot reloaded its complete admitted request"), err)
			}
			if err := store.SaveExecutionNode(ctx, tx, nodes[0], foreach); err == nil {
				return errors.New("partial inputs could overwrite the admitted node")
			}
			controls, err := store.ReadExecutionControls(ctx, tx, id, rootID, []string{parentID})
			if err != nil || len(controls) != 1 || controls[0].ElementCount != count || controls[0].NextPosition != position || controls[0].Elements != nil || controls[0].CurrentElement == nil {
				return errors.Join(errors.New("foreach did not load one element at its durable cursor"), err)
			}
			var item string
			if err := json.Unmarshal(controls[0].CurrentElement.JSON, &item); err != nil || item != items[position] {
				return errors.Join(errors.New("wrong current element after reopen/rollback"), err)
			}
			bytes, err := json.Marshal(controls[0])
			if err != nil || len(bytes) > len(items[position])+1024 {
				return errors.Join(fmt.Errorf("control read grew with the array: %d bytes", len(bytes)), err)
			}
			values, err := store.ReadExecutionNodeInputValues(ctx, tx, id, parentID, []string{"label"})
			if err != nil || len(values) != 1 || string(values["label"].JSON) != `"prefix/"` {
				return errors.Join(errors.New("static argument selection returned unrelated inputs"), err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertSnapshot(r, 0)
	var original []byte
	if err := r.Store.Pool.QueryRow(ctx, "SELECT jsonb_build_array(inputs,execution_request) FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", id, parentID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(original)
	injected := errors.New("crash before current item/child commit")
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 1); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	assertSnapshot(r, 0)
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		_, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, r.Store.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := &Runtime{Store: reopened, Client: r.Client, Host: r.Host}
	assertSnapshot(replacement, 1)
	for range 1000 {
		if err := pgx.BeginFunc(ctx, reopened.Pool, func(tx pgx.Tx) error { return replacement.advanceTx(ctx, tx, plan, id, "") }); err != nil {
			t.Fatal(err)
		}
		status, err := store.ReadRunStatus(ctx, reopened.Pool, id)
		if err != nil {
			t.Fatal(err)
		}
		if protocol.Terminal(status) {
			break
		}
	}
	run, err := reopened.Run(ctx, id)
	want, _ := json.Marshal(expected)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != string(want) {
		t.Fatalf("status=%s export bytes=%d diagnostics=%+v error=%v", run.Status, len(run.Outputs["result"]), run.Diagnostics, err)
	}
	var after []byte
	if err := reopened.Pool.QueryRow(ctx, "SELECT jsonb_build_array(inputs,execution_request) FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", id, parentID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != before {
		t.Fatal("partial coordination inputs replaced original admitted evidence")
	}
}

func TestRuntimeForeachPreservesExplicitWholeArrayArgument(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := runtimePlan("")
	integer, array := `{"type":"integer"}`, `{"type":"array","items":{"type":"integer"}}`
	body := contract.Graph{Inputs: map[string]contract.Port{"item": compositePort(integer, ""), "all": compositePort(array, "")},
		Outputs: map[string]contract.Port{"count": {Schema: json.RawMessage(integer), Bind: &contract.Binding{Expr: "size(inputs.all)"}}}}
	foreach := contract.Node{Type: "foreach", Inputs: map[string]contract.Port{"items": {Schema: json.RawMessage(array), Bind: &contract.Binding{Value: json.RawMessage(`[3,1,2]`)}}},
		Outputs: map[string]contract.Port{"count": compositePort(array, "")},
		Foreach: &contract.ForeachNode{Over: "items", Concurrency: 2, Body: body,
			With: map[string]contract.Binding{"item": {From: "iteration.item"}, "all": {From: "args.items"}}}}
	plan.Pipelines[plan.Root].Spec.Graph = contract.Graph{Nodes: map[string]contract.Node{"map": foreach},
		Outputs: map[string]contract.Port{"result": compositePort(array, "nodes.map.outputs.count")}}
	id := admitRuntimeRun(t, r, plan)
	for range 100 {
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
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `[3,3,3]` {
		t.Fatalf("status=%s result=%s diagnostics=%+v error=%v", run.Status, run.Outputs["result"], run.Diagnostics, err)
	}
}
