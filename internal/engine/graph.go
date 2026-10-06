package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func terminal(status string) bool {
	switch status {
	case "succeeded", "skipped", "failed", "cancelled":
		return true
	}

	return false
}

func (r *runtime) transition(ctx workflow.Context, node *NodeSnapshot, status string, outputs contract.Values, f *Failure, reason string) error {
	// A terminal state is immutable, even when another sibling later fails.
	if terminal(node.Status) {
		return nil
	}
	node.Status, node.Outputs, node.Failure = status, outputs, f
	if r.observability {
		now := workflow.Now(ctx)
		if node.StartedAt == nil && (status == "running" || status == "waiting_human") {
			node.StartedAt = &now
		}
		if terminal(status) {
			node.FinishedAt = &now
		}
		if reason == "" {
			switch status {
			case "ready":
				reason = "waiting for execution capacity"
			case "waiting_human":
				reason = "waiting for a human response"
			}
		}
		node.Reason = reason
	}
	if status == "succeeded" {
		r.publishing[node.ID] = true
		defer delete(r.publishing, node.ID)
	}
	return r.emit(
		ctx,
		Projection{
			Kind:       "node",
			InstanceID: node.ID,
			NodeID:     node.NodeID,
			Pipeline:   node.Pipeline,
			Status:     status,
			Attempt:    node.Attempt,
			Outputs:    outputs,
			Failure:    f,
			Reason:     reason,
		},
	)
}

func (r *runtime) graph(ctx workflow.Context, gc graphContext, graph contract.Graph, supplied contract.Values) (contract.Values, error) {
	inputs, err := contract.ValidatePorts(graph.Inputs, supplied, true)
	if err != nil {
		return nil, failure("INPUT_INVALID", err.Error())
	}
	ids := keys(graph.Nodes)
	missing := 0

	for _, id := range ids {
		if r.state.Nodes[stableID("n", gc.path+"/"+id)] == nil {
			missing++
		}
	}

	if err := r.materialize(ctx, gc.scopes, missing); err != nil {
		return nil, err
	}
	states := make(map[string]*NodeSnapshot, len(ids))
	values := make(map[string]contract.Values, len(ids))
	completed := 0

	for _, id := range ids {
		instanceID := stableID("n", gc.path+"/"+id)
		state := r.state.Nodes[instanceID]
		if state == nil {
			state = &NodeSnapshot{ID: instanceID, NodeID: id, Pipeline: gc.pipeline, Status: "pending"}
		}
		if r.observability {
			// A continuation can upgrade a checkpoint from an older history.
			// Recover ancestor addresses before materializing new descendants.
			state.ParentInstanceID, state.IterationIndex = gc.parentInstanceID, gc.iterationIndex
			state.GraphPath, state.NodeType = gc.graphPath+"/nodes/"+id, graph.Nodes[id].Type
		}
		if terminal(state.Status) {
			completed++
			values[id] = state.Outputs
		} else {
			state.Status = "pending"
		}
		states[id], r.state.Nodes[state.ID] = state, state
	}

	for _, id := range ids {
		state := states[id]
		if r.projected[state.ID] {
			continue
		}
		reason := ""
		if r.observability && len(graph.Nodes[id].Dependencies) > 0 {
			reason = "waiting for dependencies: " + strings.Join(graph.Nodes[id].Dependencies, ", ")
		}
		if err := r.emit(ctx, Projection{Kind: "node", InstanceID: state.ID, NodeID: id, Pipeline: gc.pipeline, Status: "pending", Reason: reason}); err != nil {
			return nil, err
		}
		r.projected[state.ID] = true
		if err := r.boundary(ctx); err != nil {
			return nil, err
		}
	}

	active := 0
	var graphErr error

	for completed < len(ids) {
		if ctx.Err() != nil || r.failure != nil {
			for _, id := range ids {
				if states[id].Status == "pending" && !r.checkpointing {
					if err := r.transition(ctx, states[id], "cancelled", nil, nil, "parent stopped"); err != nil && graphErr == nil {
						graphErr = err
					}
					completed++
				}
			}
			// Let every admitted child record its terminal state before closing.
			if err := workflow.Await(ctxWithoutCancel(ctx), func() bool { return active == 0 }); err != nil {
				return nil, err
			}
			if graphErr != nil {
				return nil, graphErr
			}
			if r.failure != nil {
				return nil, r.failure
			}
			return nil, ctx.Err()
		}
		started := false

		for _, id := range ids {
			state, node := states[id], graph.Nodes[id]
			if state.Status != "pending" {
				continue
			}
			ready := true

			for _, dep := range node.Dependencies {
				dependency := states[dep]
				if dependency == nil {
					return nil, failure("PLAN_INVALID", fmt.Sprintf("unknown dependency %s", dep))
				}
				if !terminal(dependency.Status) || r.publishing[dependency.ID] {
					ready = false
					break
				}
			}

			if !ready {
				continue
			}
			if len(r.paused) != 0 {
				break
			}
			state.Status = "ready"
			active++
			started = true
			workflow.Go(ctx, func(childCtx workflow.Context) {
				defer func() { active--; completed++ }()
				outputs, skipped, err := r.node(childCtx, gc, state, node, inputs, values, states)
				if r.checkpointing {
					return
				}
				if err != nil {
					f := asFailure(err)
					status := "failed"
					if childCtx.Err() != nil && (r.failure != nil || f.Code == "CANCELLED") {
						status = "cancelled"
					}
					if status == "failed" {
						if graphErr == nil {
							graphErr = err
						}
						r.stop(f)
					}
					if e := r.transition(childCtx, state, status, nil, f, ""); e != nil && graphErr == nil {
						graphErr = e
						r.stop(asFailure(e))
					}
					return
				}
				if skipped != "" {
					values[state.NodeID] = nil
					if e := r.transition(childCtx, state, "skipped", nil, nil, skipped); e != nil {
						graphErr = e
						r.stop(asFailure(e))
					}
					return
				}
				if childCtx.Err() != nil && state.Status != "succeeded" {
					_ = r.transition(childCtx, state, "cancelled", nil, nil, "parent stopped")
					return
				}
				values[state.NodeID] = outputs
				if e := r.transition(childCtx, state, "succeeded", outputs, nil, ""); e != nil {
					graphErr = e
					r.stop(asFailure(e))
				}
			})
		}

		if completed == len(ids) {
			break
		}
		if active == 0 && !started && len(r.paused) == 0 {
			return nil, failure("PLAN_INVALID", "graph cannot make progress")
		}
		before := completed
		if err := workflow.Await(
			ctx,
			func() bool { return completed != before || (len(r.paused) == 0 && active == 0) || r.failure != nil },
		); err != nil {
			continue
		}
	}

	if graphErr != nil {
		return nil, graphErr
	}
	if r.failure != nil {
		return nil, r.failure
	}
	return bindPorts(graph.Outputs, contract.Scope{Inputs: inputs, Nodes: values}, false)
}

func bindPorts(ports map[string]contract.Port, scope contract.Scope, skipMissing bool) (contract.Values, error) {
	return execution.BindPorts(ports, scope, skipMissing)
}

func (r *runtime) node(
	ctx workflow.Context,
	gc graphContext,
	state *NodeSnapshot,
	node contract.Node,
	inputs contract.Values,
	values map[string]contract.Values,
	siblings map[string]*NodeSnapshot,
) (contract.Values, string, error) {
	if err := r.admit(ctx); err != nil {
		return nil, "", err
	}

	for _, dependency := range node.Needs {
		if siblings[dependency] == nil || siblings[dependency].Status != "succeeded" {
			return nil, "needs dependency did not succeed", nil
		}
	}

	args, err := bindPorts(node.Inputs, contract.Scope{Inputs: inputs, Nodes: values}, true)
	if err != nil {
		return nil, "", err
	}
	if args == nil {
		return nil, "required input is missing", nil
	}
	if r.observability {
		state.Inputs, state.DataTruncated = observationInputs(args)
	}
	if node.When != "" {
		enabled, present, err := contract.EvalBool(node.When, contract.Scope{Inputs: inputs, Nodes: values, Args: args})
		if err != nil {
			return nil, "", failure("CONDITION_INVALID", err.Error())
		}
		if !present {
			return nil, "condition dependency is missing", nil
		}
		if !enabled {
			return nil, "condition is false", nil
		}
	}
	node.Execution = executionPolicy(gc.document.Spec.Defaults.Execution, node.Execution)
	deadline := nodeDeadline(workflow.Now(ctx), gc.deadline, node)
	if !state.Deadline.IsZero() {
		deadline = state.Deadline
	} else {
		state.Deadline = deadline
	}
	if !deadline.After(workflow.Now(ctx)) {
		return nil, "", failure("DEADLINE_EXCEEDED", "node deadline exceeded")
	}
	nodeCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	status := "running"

	switch node.Type {
	case "llm", "agent", "code", "tool":
		status = "ready"
	}

	if err := r.transition(ctx, state, status, nil, nil, ""); err != nil {
		return nil, "", err
	}
	// Projection activity backpressure bounds new timer commands per workflow
	// task. The deadline was fixed above, so waiting for persistence never grants
	// additional execution time and an expired node cannot start external work.
	if !deadline.After(workflow.Now(ctx)) {
		return nil, "", failure("DEADLINE_EXCEEDED", "node deadline exceeded during admission")
	}
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	timedOut := false
	workflow.Go(timerCtx, func(tctx workflow.Context) {
		if timer(tctx, deadline.Sub(workflow.Now(tctx)), "Node deadline: "+state.NodeID) == nil {
			timedOut = true
			cancel()
		}
	})
	gc.deadline = deadline
	var outputs contract.Values

	switch node.Type {
	case "llm", "agent", "code", "tool":
		outputs, err = r.leaf(nodeCtx, gc, state, node, args)
	case "switch":
		outputs, err = runSwitch(node, args)
	case "human":
		outputs, err = r.human(nodeCtx, gc, state, node, args)
	case "foreach":
		outputs, err = r.foreach(nodeCtx, gc, state, node, args)
	case "loop":
		outputs, err = r.loop(nodeCtx, gc, state, node, args)
	case "pipeline":
		outputs, err = r.pipeline(nodeCtx, gc, state, node, args)
	default:
		err = failure("PLAN_INVALID", "unknown node type: "+node.Type)
	}

	if timedOut && state.Status != "succeeded" {
		return nil, "", failure("DEADLINE_EXCEEDED", "node deadline exceeded")
	}
	if err != nil {
		return nil, "", err
	}
	if nodeCtx.Err() != nil && state.Status != "succeeded" {
		return nil, "", nodeCtx.Err()
	}
	outputs, err = contract.ValidatePorts(node.Outputs, outputs, false)
	if err != nil {
		return nil, "", failure("OUTPUT_INVALID", err.Error())
	}
	return withoutArtifactPaths(outputs), "", nil
}

// A sandbox path is attempt-local context, never a published artifact property.
func withoutArtifactPaths(values contract.Values) contract.Values {
	return execution.WithoutArtifactPaths(values)
}

func executionPolicy(defaults, own contract.Execution) contract.Execution {
	return execution.ExecutionPolicy(defaults, own)
}

func nodeDeadline(now, parent time.Time, node contract.Node) time.Time {
	return execution.NodeDeadline(now, parent, node)
}

// Bound extra checkpoint/projection context without changing executable values.
// The API marks omitted ports explicitly instead of presenting partial JSON as valid.
func observationInputs(values contract.Values) (contract.Values, bool) {
	result := contract.Values{}
	size, truncated := 0, false
	for _, key := range keys(values) {
		value := withoutArtifactPaths(contract.Values{key: values[key]})[key]
		encoded, err := json.Marshal(value)
		if err != nil || size+len(key)+len(encoded) > 32<<10 {
			truncated = true
			continue
		}
		size += len(key) + len(encoded)
		result[key] = value
	}
	return result, truncated
}
