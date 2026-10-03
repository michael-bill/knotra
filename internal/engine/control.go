package engine

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/michael-bill/knotra/internal/contract"
	"go.temporal.io/sdk/workflow"
)

func jsonValue(value any) contract.Value {
	raw, _ := json.Marshal(value)
	return contract.Value{JSON: raw}
}

func runSwitch(node contract.Node, args contract.Values) (contract.Values, error) {
	if node.Switch == nil {
		return nil, failure("PLAN_INVALID", "missing switch configuration")
	}
	route := node.Switch.Default
	for _, branch := range node.Switch.Cases {
		match, present, err := contract.EvalBool(branch.When, contract.Scope{Args: args})
		if err != nil {
			return nil, failure("CONDITION_INVALID", err.Error())
		}
		if !present {
			return nil, failure("CONDITION_INVALID", "switch condition is missing")
		}
		if match {
			route = branch.Name
			break
		}
	}
	return contract.Values{"route": jsonValue(route)}, nil
}

func bindWith(bindings map[string]contract.Binding, scope contract.Scope) (contract.Values, error) {
	values := contract.Values{}
	for _, name := range keys(bindings) {
		value, present, err := contract.EvalBinding(bindings[name], scope)
		if err != nil {
			return nil, failure("BINDING_INVALID", name+": "+err.Error())
		}
		if present {
			values[name] = value
		}
	}
	return values, nil
}

func items(value contract.Value) ([]contract.Value, error) {
	if value.Collection {
		result := make([]contract.Value, len(value.Artifacts))
		for i, artifact := range value.Artifacts {
			result[i] = contract.Value{Artifacts: []contract.Artifact{artifact}}
		}
		return result, nil
	}
	if value.Artifacts != nil {
		return nil, failure("INPUT_INVALID", "foreach requires an artifact collection")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(value.JSON, &raw); err != nil || string(value.JSON) == "null" {
		return nil, failure("INPUT_INVALID", "foreach requires a JSON array")
	}
	result := make([]contract.Value, len(raw))
	for i, v := range raw {
		result[i] = contract.Value{JSON: v}
	}
	return result, nil
}

func (r *runtime) foreach(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, args contract.Values) (contract.Values, error) {
	config := node.Foreach
	if config == nil || config.Concurrency < 1 {
		return nil, failure("PLAN_INVALID", "invalid foreach configuration")
	}
	value, present := args[config.Over]
	if !present {
		return nil, failure("INPUT_INVALID", "foreach input is absent")
	}
	elements, err := items(value)
	if err != nil {
		return nil, err
	}
	results := make([]contract.Values, len(elements))
	active, completed, next := 0, 0, 0
	if r.foreachResults[state.ID] == nil {
		r.foreachResults[state.ID] = map[int]contract.Values{}
	}
	for index, result := range r.foreachResults[state.ID] {
		results[index] = result
		completed++
	}
	var bodyErr error
	for completed < len(elements) {
		if ctx.Err() != nil || bodyErr != nil || r.failure != nil {
			_ = workflow.Await(ctxWithoutCancel(ctx), func() bool { return active == 0 })
			if bodyErr != nil {
				return nil, bodyErr
			}
			if r.failure != nil {
				return nil, r.failure
			}
			return nil, ctx.Err()
		}
		for next < len(elements) && active < config.Concurrency && len(r.paused) == 0 {
			if results[next] != nil {
				next++
				continue
			}
			index := next
			next++
			active++
			workflow.Go(ctx, func(childCtx workflow.Context) {
				defer func() { active--; completed++ }()
				with, err := bindWith(config.With, contract.Scope{Args: args, IterationItem: &elements[index], IterationIndex: index})
				if err == nil {
					child := gc
					child.path = state.ID + "/item/" + strconv.Itoa(index)
					results[index], err = r.graph(childCtx, child, config.Body, with)
					if err == nil {
						r.foreachResults[state.ID][index] = results[index]
					}
				}
				if err != nil && bodyErr == nil {
					bodyErr = err
					if !r.checkpointing {
						r.stop(asFailure(err))
					}
				}
			})
		}
		before := completed
		if err := workflow.Await(ctx, func() bool {
			return completed != before || bodyErr != nil || (len(r.paused) == 0 && active < config.Concurrency && next < len(elements))
		}); err != nil {
			continue
		}
	}
	if bodyErr != nil {
		return nil, bodyErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	output := contract.Values{}
	for _, name := range keys(config.Body.Outputs) {
		port := config.Body.Outputs[name]
		if port.Artifact != nil {
			value := contract.Value{Collection: true, Artifacts: make([]contract.Artifact, 0, len(results))}
			for _, result := range results {
				v, present := result[name]
				if !present || v.Collection || len(v.Artifacts) != 1 {
					return nil, failure("OUTPUT_INVALID", "foreach body must export a single artifact at "+name)
				}
				value.Artifacts = append(value.Artifacts, v.Artifacts[0])
			}
			output[name] = value
		} else {
			values := make([]json.RawMessage, 0, len(results))
			for _, result := range results {
				value, present := result[name]
				if !present {
					return nil, failure("OUTPUT_UNAVAILABLE", "foreach body output missing: "+name)
				}
				values = append(values, value.JSON)
			}
			output[name] = jsonValue(values)
		}
	}
	return output, nil
}

func loopState(ports map[string]contract.Port, scope contract.Scope, initial bool) (contract.Values, error) {
	result := contract.Values{}
	for _, name := range keys(ports) {
		port := ports[name]
		binding := port.Next
		if initial {
			binding = port.Initial
		}
		if binding == nil {
			return nil, failure("PLAN_INVALID", "loop state binding absent: "+name)
		}
		value, present, err := contract.EvalBinding(*binding, scope)
		if err != nil {
			return nil, failure("BINDING_INVALID", err.Error())
		}
		if !present {
			return nil, failure("STATE_UNAVAILABLE", "loop state missing: "+name)
		}
		if err := contract.ValidateValue(port, value); err != nil {
			return nil, failure("VALUE_INVALID", name+": "+err.Error())
		}
		result[name] = value
	}
	return result, nil
}

func (r *runtime) loop(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, args contract.Values) (contract.Values, error) {
	config := node.Loop
	if config == nil || config.MaxIterations < 1 {
		return nil, failure("PLAN_INVALID", "invalid loop configuration")
	}
	carry, err := loopState(config.State, contract.Scope{Args: args}, true)
	if err != nil {
		return nil, err
	}
	firstIndex := 0
	if saved, ok := r.loops[state.ID]; ok {
		firstIndex, carry = saved.Index, saved.State
	}
	for index := firstIndex; index < config.MaxIterations; index++ {
		r.loops[state.ID] = LoopCheckpoint{Index: index, State: carry}
		if err := r.admit(ctx); err != nil {
			return nil, err
		}
		scope := contract.Scope{Args: args, State: carry, IterationIndex: index}
		with, err := bindWith(config.With, scope)
		if err != nil {
			return nil, err
		}
		child := gc
		child.path = state.ID + "/iteration/" + strconv.Itoa(index)
		outputs, err := r.graph(ctx, child, config.Body, with)
		if err != nil {
			return nil, err
		}
		scope.Body = outputs
		done, present, err := contract.EvalBool(config.Until, scope)
		if err != nil {
			return nil, failure("CONDITION_INVALID", err.Error())
		}
		if !present {
			return nil, failure("CONDITION_INVALID", "loop condition is missing")
		}
		if done || index+1 == config.MaxIterations {
			termination := "condition"
			if !done {
				if config.OnLimit != "return_last" {
					return nil, failure("LIMIT_EXCEEDED", "loop iteration limit reached")
				}
				termination = "limit"
			}
			result := contract.Values{}
			for _, key := range keys(outputs) {
				result[key] = outputs[key]
			}
			result["iterations"], result["termination"] = jsonValue(index+1), jsonValue(termination)
			return result, nil
		}
		carry, err = loopState(config.State, scope, false)
		if err != nil {
			return nil, err
		}
		r.loops[state.ID] = LoopCheckpoint{Index: index + 1, State: carry}
	}
	return nil, failure("PLAN_INVALID", "loop did not execute")
}

func (r *runtime) pipeline(ctx workflow.Context, gc graphContext, state *NodeSnapshot, node contract.Node, args contract.Values) (contract.Values, error) {
	if node.Pipeline == nil {
		return nil, failure("PLAN_INVALID", "missing pipeline configuration")
	}
	document := r.in.Plan.Pipelines[node.Pipeline.File]
	if document == nil {
		return nil, failure("PLAN_INVALID", fmt.Sprintf("pipeline %q not in admitted package", node.Pipeline.File))
	}
	limits := restrictLimits(gc.scopes[len(gc.scopes)-1].Limits, document.Spec.Limits)
	child := graphContext{pipeline: node.Pipeline.File, document: document, path: state.ID + "/pipeline", scopes: append(append([]BudgetScope(nil), gc.scopes...), BudgetScope{ID: state.ID, Limits: limits}), permissions: intersectPermissions(gc.permissions, &node.Pipeline.Permissions), deadline: gc.deadline}
	if document.Spec.Limits.Timeout != "" {
		duration, err := contract.Duration(document.Spec.Limits.Timeout)
		if err != nil {
			return nil, failure("PLAN_INVALID", "invalid child deadline")
		}
		deadline := workflow.Now(ctx).Add(duration)
		if deadline.Before(child.deadline) {
			child.deadline = deadline
		}
	}
	if !state.ChildDeadline.IsZero() {
		child.deadline = state.ChildDeadline
	} else {
		state.ChildDeadline = child.deadline
	}
	childCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	timedOut := false
	workflow.Go(timerCtx, func(tctx workflow.Context) {
		if timer(tctx, child.deadline.Sub(workflow.Now(tctx)), "Child pipeline deadline: "+state.NodeID) == nil {
			timedOut = true
			cancel()
		}
	})
	outputs, err := r.graph(childCtx, child, document.Spec.Graph, args)
	if timedOut {
		return nil, failure("DEADLINE_EXCEEDED", "child pipeline deadline exceeded")
	}
	return outputs, err
}

func intersectPermissions(parent, child *contract.Permissions) *contract.Permissions {
	if parent == nil {
		copy := *child
		return &copy
	}
	result := &contract.Permissions{Models: intersect(parent.Models, child.Models), Sandboxes: intersect(parent.Sandboxes, child.Sandboxes), Secrets: intersect(parent.Secrets, child.Secrets), MCP: map[string][]string{}}
	for _, name := range keys(child.MCP) {
		result.MCP[name] = intersect(parent.MCP[name], child.MCP[name])
	}
	return result
}

func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, value := range a {
		set[value] = true
	}
	result := []string{}
	for _, value := range b {
		if set[value] {
			result = append(result, value)
		}
	}
	return result
}
