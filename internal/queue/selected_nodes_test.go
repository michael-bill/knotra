package queue

import (
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

func TestRuntimeFocusedGraphPreservesScanAndRecoversSiblingAnswers(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := foreachPlan("", true)
	id := admitRuntimeRun(t, r, plan)
	current := r
	advance := func(focus string) error {
		return pgx.BeginFunc(ctx, current.Store.Pool, func(tx pgx.Tx) error {
			return current.advanceTx(ctx, tx, plan, id, focus)
		})
	}
	for range 10 {
		if err := advance(""); err != nil {
			t.Fatal(err)
		}
	}
	parent := execution.StableID("n", id+"/root/map")
	instance := func(index int) string { return execution.StableID("n", fmt.Sprintf("%s/item/%d/work", parent, index)) }
	answer := func(index, value int) {
		t.Helper()
		request := execution.StableID("h", instance(index)+"/human")
		if err := pgx.BeginFunc(ctx, current.Store.Pool, func(tx pgx.Tx) error {
			return store.Respond(ctx, tx, request, request, contract.Values{"value": {JSON: json.RawMessage(fmt.Sprint(value))}}, current.Wake)
		}); err != nil {
			t.Fatal(err)
		}
	}
	answer(0, 3)
	answer(1, 1)
	var cursor string
	if err := r.Store.Pool.QueryRow(ctx, `SELECT g.address FROM knotra_execution_graphs g
		JOIN knotra_execution_nodes n ON n.run_id=g.run_id AND n.graph_id=g.id WHERE n.run_id=$1 AND n.id=$2`, id, instance(1)).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE knotra_runs SET graph_cursor=$2,graph_scan_again=false WHERE id=$1", id, cursor); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("publication rollback")
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if err := r.advanceTx(ctx, tx, plan, id, instance(0)); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	assertState := func(wantFocus string, wantAgain bool) {
		t.Helper()
		var focus, sibling, gotCursor string
		var again bool
		if err := r.Store.Pool.QueryRow(ctx, `SELECT a.state,b.state,r.graph_cursor,r.graph_scan_again
			FROM knotra_runs r JOIN knotra_execution_nodes a ON a.run_id=r.id AND a.id=$2
			JOIN knotra_execution_nodes b ON b.run_id=r.id AND b.id=$3 WHERE r.id=$1`, id, instance(0), instance(1)).Scan(&focus, &sibling, &gotCursor, &again); err != nil {
			t.Fatal(err)
		}
		if focus != wantFocus || sibling != "waiting_human" || gotCursor != cursor || again != wantAgain {
			t.Fatalf("focused scan changed sibling/cursor: focus=%s sibling=%s cursor=%s again=%v", focus, sibling, gotCursor, again)
		}
	}
	assertState("waiting_human", false)
	if err := advance(instance(0)); err != nil {
		t.Fatal(err)
	}
	assertState("succeeded", true)
	reopened, err := store.Open(ctx, r.Store.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current = &Runtime{Store: reopened, Client: r.Client, Host: r.Host}
	for range 10 {
		if err := advance(""); err != nil {
			t.Fatal(err)
		}
	}
	answer(2, 2)
	for range 10 {
		if err := advance(""); err != nil {
			t.Fatal(err)
		}
	}
	run, err := reopened.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != "[3,1,2]" || len(run.Instances) != 4 {
		t.Fatalf("sibling/parent recovery changed run: %+v error=%v", run, err)
	}
}

func TestRuntimeLargeGraphSelectsBoundedNodesAndRecoversReadiness(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := runtimePlan("")
	plan.Profile.Spec.Limits.MaxNodeInstances = 1100
	plan.Profile.Spec.Limits.Timeout = "5m"
	text := `{"type":"string"}`
	graph := contract.Graph{Nodes: map[string]contract.Node{}, Outputs: map[string]contract.Port{
		"result":   compositePort(text, "nodes.z_join.outputs.route"),
		"complete": compositePort(text, "nodes.a0000.outputs.route"),
	}}
	const count = 1004
	full := strings.Repeat("full", 50000)
	dependencies := make([]string, count)
	for i := range count {
		name := fmt.Sprintf("a%04d", i)
		dependencies[i] = name
		value := "small"
		if i == 0 {
			value = full
		}
		graph.Nodes[name] = contract.Node{Type: "switch", Switch: &contract.SwitchNode{Default: value},
			Outputs: map[string]contract.Port{"route": compositePort(text, "")}}
	}
	input := compositePort(text, "")
	input.Bind = &contract.Binding{Coalesce: []contract.Binding{{From: "nodes.a0000.outputs.route"}, {From: "nodes.a1003.outputs.route"}}}
	graph.Nodes["z_join"] = contract.Node{Type: "switch", Dependencies: dependencies, Needs: dependencies,
		Inputs: map[string]contract.Port{"value": input}, Outputs: map[string]contract.Port{"route": compositePort(text, "")},
		Switch: &contract.SwitchNode{Cases: []contract.SwitchCase{{Name: "complete", When: "size(args.value) == 200000"}}, Default: "truncated"}}
	plan.Pipelines[plan.Root].Spec.Graph = graph
	id := admitRuntimeRun(t, r, plan)
	rootID := execution.StableID("g", id+"/root")
	joinID := execution.StableID("n", id+"/root/z_join")
	// Finish only materialization, so the initial readiness count is exact.
	for remaining := count + 1; remaining > 0; {
		bound := min(64, remaining)
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
			changes, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", bound)
			if err == nil && changes.Transitions != bound {
				return fmt.Errorf("materialized transitions=%d bound=%d", changes.Transitions, bound)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		remaining -= bound
	}
	assertReadiness := func(want int) {
		t.Helper()
		var pending int
		if err := r.Store.Pool.QueryRow(ctx, "SELECT remaining_dependencies FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2", id, joinID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != want {
			t.Fatalf("readiness=%d want=%d", pending, want)
		}
	}
	assertReadiness(count)
	// One bounded admission page, then roll back a complete result page.
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		_, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 64)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("crash before result/readiness commit")
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 64); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	assertReadiness(count)
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		_, err := r.advanceGraphTx(ctx, tx, plan, id, rootID, "", 64)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertReadiness(count - 64)
	// A duplicate terminal transition cannot release the same edges again.
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		values, err := store.ReadExecutionDependencyValues(ctx, tx, id, rootID, []string{"a0000"})
		if err != nil {
			return err
		}
		duplicate := values[0]
		duplicate.Revision = 100
		if err := store.SaveExecutionNode(ctx, tx, duplicate, graph.Nodes[duplicate.NodeID]); !errors.Is(err, store.ErrConflict) {
			return errors.Join(errors.New("duplicate terminal transition changed readiness"), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertReadiness(count - 64)
	reopened, err := store.Open(ctx, r.Store.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := &Runtime{Store: reopened, Client: r.Client, Host: r.Host}
	completed := false
	for range 100 {
		if err := pgx.BeginFunc(ctx, reopened.Pool, func(tx pgx.Tx) error {
			run, err := store.LockExecutionRun(ctx, tx, id)
			if err != nil {
				return err
			}
			root, err := store.ReadExecutionGraph(ctx, tx, id, rootID)
			if err != nil {
				return err
			}
			// Completion means all 1,004 upstream states, not just the selected page.
			var terminal int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_nodes WHERE run_id=$1 AND state='succeeded' AND id<>$2", id, joinID).Scan(&terminal); err != nil {
				return err
			}
			completed = terminal == count
			nodes, err := store.ReadSchedulableNodes(ctx, tx, run, root, "", 10000)
			if err != nil {
				return err
			}
			if len(nodes) > 64 {
				return fmt.Errorf("loaded %d full node records", len(nodes))
			}
			for _, node := range nodes {
				if protocol.Terminal(node.State) || (node.NodeID == "z_join" && !completed) {
					return fmt.Errorf("unaffected or blocked node selected: %s/%s", node.NodeID, node.State)
				}
			}
			return replacement.advanceTx(ctx, tx, plan, id, "")
		}); err != nil {
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
	var output string
	if err == nil {
		err = json.Unmarshal(run.Outputs["complete"], &output)
	}
	if err != nil || run.Status != "succeeded" || string(run.Outputs["result"]) != `"complete"` || output != full {
		t.Fatalf("status=%s result=%s output bytes=%d diagnostics=%+v error=%v", run.Status, run.Outputs["result"], len(output), run.Diagnostics, err)
	}
	assertReadiness(0)
}

func TestRuntimeNodeSelectionSkipsIdleHumanWaitsAndCancelsInPages(t *testing.T) {
	r := newTestRuntime(t)
	ctx := t.Context()
	plan := runtimePlan("")
	text := `{"type":"string"}`
	graph := contract.Graph{Nodes: map[string]contract.Node{}}
	human := contract.Node{Type: "human", Human: &contract.HumanNode{Prompt: contract.TextSource{Text: "Answer"}},
		Outputs: map[string]contract.Port{"answer": compositePort(text, "")}}
	for i := range 70 {
		graph.Nodes[fmt.Sprintf("a%02d", i)] = human
	}
	graph.Nodes["z_source"] = contract.Node{Type: "switch", Switch: &contract.SwitchNode{Default: "later"},
		Outputs: map[string]contract.Port{"route": compositePort(text, "")}}
	human.Dependencies = []string{"z_source"}
	human.Inputs = map[string]contract.Port{"value": compositePort(text, "nodes.z_source.outputs.route")}
	graph.Nodes["zz_dependent"] = human
	plan.Pipelines[plan.Root].Spec.Graph = graph
	id := admitRuntimeRun(t, r, plan)
	rootID := execution.StableID("g", id+"/root")
	for range 10 {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") }); err != nil {
			t.Fatal(err)
		}
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		run, err := store.LockExecutionRun(ctx, tx, id)
		if err != nil {
			return err
		}
		root, err := store.ReadExecutionGraph(ctx, tx, id, rootID)
		if err != nil {
			return err
		}
		// Retained decisions for an instance must never make its current open
		// wait appear answered or crowd later eligible work out of the page.
		for i := range 70 {
			instance := execution.StableID("n", fmt.Sprintf("%s/root/a%02d", id, i))
			if err := r.Store.SaveRequestTx(ctx, tx, execution.Request{RunID: id, ID: "old/" + instance,
				InstanceID: instance, Kind: "resolution", Status: "resolved", Deadline: run.Deadline}); err != nil {
				return err
			}
		}
		nodes, err := store.ReadSchedulableNodes(ctx, tx, run, root, "", 64)
		if err != nil || len(nodes) != 0 {
			return errors.Join(fmt.Errorf("idle graph selected %d records", len(nodes)), err)
		}
		var waits int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM knotra_execution_nodes WHERE run_id=$1 AND state='waiting_human'", id).Scan(&waits); err != nil {
			return err
		}
		if waits != 71 || root.State != "running" {
			return fmt.Errorf("later node starved or partial graph completed: waits=%d graph=%s", waits, root.State)
		}
		instance := execution.StableID("n", id+"/root/a69")
		requests, err := store.ReadExecutionRequests(ctx, tx, id, rootID, []string{instance})
		if err != nil || len(requests) != 1 || requests[0].Status != "open" {
			return errors.Join(fmt.Errorf("loaded historical waits: %+v", requests), err)
		}
		return store.Respond(ctx, tx, requests[0].ID, "accepted", contract.Values{"answer": {JSON: json.RawMessage(`"valid"`)}}, r.CommandWake)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, "SELECT state FROM knotra_execution_nodes WHERE run_id=$1 AND node_id='a69'", id).Scan(&state); err != nil {
			return err
		}
		if state != "succeeded" {
			return fmt.Errorf("accepted current answer did not get its transition: %s", state)
		}
		return store.Cancel(ctx, tx, id, r.CommandWake)
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error { return r.advanceTx(ctx, tx, plan, id, "") }); err != nil {
			t.Fatal(err)
		}
	}
	run, err := r.Store.Run(ctx, id)
	var open, active int
	if err == nil {
		err = r.Store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM knotra_requests WHERE run_id=$1 AND status='open'),
			(SELECT count(*) FROM knotra_execution_nodes WHERE run_id=$1 AND state NOT IN ('succeeded','skipped','failed','cancelled'))`, id).Scan(&open, &active)
	}
	if err != nil || run.Status != "cancelled" || open != 0 || active != 0 {
		t.Fatalf("status=%s open=%d active=%d error=%v", run.Status, open, active, err)
	}
}
