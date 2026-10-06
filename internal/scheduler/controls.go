package scheduler

import (
	"encoding/json"
	"strconv"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

func composite(kind string) bool { return kind == "pipeline" || kind == "foreach" || kind == "loop" }

// advanceControl performs at most one persisted coordination transition. The
// admitted parent deadline and frozen child inputs survive every later delivery.
func advanceControl(plan contract.Plan, run execution.RunState, graph execution.GraphRecord, node execution.NodeRecord,
	control execution.ControlRecord, children []execution.GraphRecord, collected map[string]contract.Values, changes *Changes) (*execution.ExecuteResult, bool) {
	definition := node.Request.Node
	if control.Revision == 0 {
		control = execution.ControlRecord{RunID: run.RunID, InstanceID: node.ID, Kind: definition.Type}
		switch definition.Type {
		case "foreach":
			if definition.Foreach == nil || definition.Foreach.Concurrency < 1 {
				return failedControl("PLAN_INVALID", "invalid foreach configuration"), false
			}
			value, found := node.Inputs[definition.Foreach.Over]
			if !found {
				return failedControl("INPUT_INVALID", "foreach input is absent"), false
			}
			var err error
			control.Elements, err = execution.Items(value)
			if err != nil {
				return &execution.ExecuteResult{Failure: asFailure(err)}, false
			}
			control.ElementCount, control.Concurrency = len(control.Elements), definition.Foreach.Concurrency
		case "loop":
			if definition.Loop == nil || definition.Loop.MaxIterations < 1 {
				return failedControl("PLAN_INVALID", "invalid loop configuration"), false
			}
			var err error
			control.Carry, err = execution.LoopState(definition.Loop.State, contract.Scope{Args: node.Inputs}, true)
			if err != nil {
				return &execution.ExecuteResult{Failure: asFailure(err)}, false
			}
		case "pipeline":
			if definition.Pipeline == nil || plan.Pipelines[definition.Pipeline.File] == nil {
				return failedControl("PLAN_INVALID", "child pipeline is not in the frozen plan"), false
			}
		}
		control.Revision = 1
		changes.Controls = append(changes.Controls, control)
		changes.More = true
		return nil, true
	}
	if control.RunID != node.RunID || control.InstanceID != node.ID || control.Kind != definition.Type {
		return failedControl("PLAN_INVALID", "control does not match its admitted instance"), false
	}
	if control.ElementCount < 0 || control.NextPosition < 0 || control.NextPosition > control.ElementCount || control.CollectionPosition < 0 || control.CollectionPosition > control.NextPosition || control.Iteration < 0 {
		return failedControl("PLAN_INVALID", "control position is inconsistent"), false
	}
	var child *execution.GraphRecord
	active := control.ActiveChildren
	for i := range children {
		if children[i].ParentInstanceID != node.ID {
			continue
		}
		if terminal(children[i].State) && children[i].State != "succeeded" {
			if children[i].Failure == nil {
				return failedControl("EXECUTION_FAILED", "child graph failed without a cause"), false
			}
			return &execution.ExecuteResult{Failure: children[i].Failure}, false
		}
		if children[i].ID == control.ChildGraphID {
			child = &children[i]
		}
	}
	if control.ChildGraphID != "" && child == nil {
		return failedControl("PLAN_INVALID", "admitted child graph is missing"), false
	}
	if definition.Type == "foreach" {
		if control.Concurrency != definition.Foreach.Concurrency {
			return failedControl("PLAN_INVALID", "foreach concurrency differs from its admitted definition"), false
		}
		if control.ActiveChildren < 0 || control.SucceededChildren < 0 || control.FailedChildren < 0 || control.ActiveChildren+control.SucceededChildren+control.FailedChildren != control.NextPosition {
			return failedControl("PLAN_INVALID", "foreach child summary is inconsistent"), false
		}
		if control.NextPosition == control.ElementCount && active == 0 {
			if control.SucceededChildren != control.ElementCount {
				return failedControl("PLAN_INVALID", "successful foreach child is missing"), false
			}
			if control.CollectionPosition < control.ElementCount {
				position, err := validateForeachPage(definition.Foreach.Body, node.ID, control.CollectionPosition, control.ElementCount, children)
				if err != nil {
					return &execution.ExecuteResult{Failure: asFailure(err)}, false
				}
				control.CollectionPosition = position
				control.Revision++
				changes.Controls = append(changes.Controls, control)
				changes.More = true
				return nil, true
			}
			outputs, present := collected[node.ID]
			if !present {
				changes.ForeachCollections = append(changes.ForeachCollections, node.ID)
				return nil, true
			}
			return &execution.ExecuteResult{Outputs: outputs}, false
		}
		if run.Paused || active >= control.Concurrency || control.NextPosition == control.ElementCount {
			return nil, false
		}
	} else if child != nil {
		if child.State != "succeeded" {
			return nil, false
		}
		if definition.Type == "pipeline" {
			return &execution.ExecuteResult{Outputs: child.Outputs}, false
		}
		scope := contract.Scope{Args: node.Inputs, State: control.Carry, Body: child.Outputs, IterationIndex: control.Iteration}
		done, present, err := contract.EvalBool(definition.Loop.Until, scope)
		if err != nil {
			return failedControl("CONDITION_INVALID", err.Error()), false
		}
		if !present {
			return failedControl("CONDITION_INVALID", "loop condition is missing"), false
		}
		if done || control.Iteration+1 == definition.Loop.MaxIterations {
			termination := "condition"
			if !done {
				if definition.Loop.OnLimit != "return_last" {
					return failedControl("LIMIT_EXCEEDED", "loop iteration limit reached"), false
				}
				termination = "limit"
			}
			outputs := make(contract.Values, len(child.Outputs)+2)
			for name, value := range child.Outputs {
				outputs[name] = value
			}
			outputs["iterations"], outputs["termination"] = controlValue(control.Iteration+1), controlValue(termination)
			return &execution.ExecuteResult{Outputs: outputs}, false
		}
		// Evaluate every next value against the same old carry, then replace it.
		carry, err := execution.LoopState(definition.Loop.State, scope, false)
		if err != nil {
			return &execution.ExecuteResult{Failure: asFailure(err)}, false
		}
		control.Carry, control.Iteration, control.ChildGraphID = carry, control.Iteration+1, ""
		control.Revision++
		changes.Controls = append(changes.Controls, control)
		changes.More = true
		return nil, true
	}
	if run.Paused {
		return nil, false
	}
	newChild, scope, err := controlChild(plan, graph, node, control)
	if err != nil {
		return &execution.ExecuteResult{Failure: asFailure(err)}, false
	}
	if scope != nil {
		changes.NewScopes = append(changes.NewScopes, *scope)
	}
	if definition.Type == "foreach" {
		control.NextPosition++
	} else {
		control.ChildGraphID = newChild.ID
	}
	control.Revision++
	changes.Controls = append(changes.Controls, control)
	changes.Children = append(changes.Children, newChild)
	changes.Timers = append(changes.Timers, execution.TimerRecord{RunID: node.RunID, ID: "graph/" + newChild.ID,
		Generation: 1, InstanceID: node.ID, Kind: "node_deadline", DueAt: newChild.Deadline})
	changes.More = true
	return nil, true
}

func validateForeachPage(body contract.Graph, parentID string, position, count int, children []execution.GraphRecord) (int, error) {
	start := position
	for _, child := range children {
		if child.ParentInstanceID != parentID {
			continue
		}
		if position-start == 64 {
			break
		}
		if child.IterationIndex == nil || *child.IterationIndex != position || position >= count || child.State != "succeeded" {
			return start, failure("PLAN_INVALID", "foreach child position is inconsistent")
		}
		for name, port := range body.Outputs {
			value, present := child.Outputs[name]
			if !present {
				return start, failure("OUTPUT_UNAVAILABLE", "foreach body output missing: "+name)
			}
			if port.Artifact != nil && (value.Collection || len(value.Artifacts) != 1) {
				return start, failure("OUTPUT_INVALID", "foreach body must export a single artifact at "+name)
			}
		}
		position++
	}
	if position == start {
		return start, failure("PLAN_INVALID", "admitted foreach child is missing")
	}
	return position, nil
}

func controlChild(plan contract.Plan, graph execution.GraphRecord, node execution.NodeRecord, control execution.ControlRecord) (execution.GraphRecord, *execution.ScopeRecord, error) {
	child := execution.GraphRecord{RunID: node.RunID, ParentInstanceID: node.ID, Pipeline: graph.Pipeline,
		Permissions: graph.Permissions, Scopes: graph.Scopes, Deadline: node.Deadline, State: "pending"}
	definition := node.Request.Node
	var scope *execution.ScopeRecord
	var err error
	switch definition.Type {
	case "pipeline":
		document := plan.Pipelines[definition.Pipeline.File]
		if len(graph.Scopes) == 0 {
			return child, nil, failure("PLAN_INVALID", "child pipeline has no enclosing scope")
		}
		limits := execution.RestrictLimits(graph.Scopes[len(graph.Scopes)-1].Limits, document.Spec.Limits)
		scope = &execution.ScopeRecord{RunID: node.RunID, ID: node.ID, Limits: limits}
		child.Address, child.Pipeline, child.Inputs = node.ID+"/pipeline", definition.Pipeline.File, node.Inputs
		child.Scopes = append(append([]execution.BudgetScope(nil), graph.Scopes...), execution.BudgetScope{ID: node.ID, Limits: limits})
		child.Permissions = execution.IntersectPermissions(graph.Permissions, &definition.Pipeline.Permissions)
		if document.Spec.Limits.Timeout != "" {
			duration, e := contract.Duration(document.Spec.Limits.Timeout)
			if e != nil || node.StartedAt == nil {
				return child, nil, failure("PLAN_INVALID", "invalid child deadline")
			}
			deadline := node.StartedAt.Add(duration)
			if deadline.Before(child.Deadline) {
				child.Deadline = deadline
			}
		}
	case "foreach":
		index := control.NextPosition
		if control.CurrentElement == nil {
			return child, nil, failure("PLAN_INVALID", "current foreach element is missing")
		}
		child.IterationIndex = &index
		child.Address = node.ID + "/item/" + strconv.Itoa(index)
		child.GraphPath = graph.GraphPath + "/nodes/" + node.NodeID + "/body"
		child.Inputs, err = execution.BindWith(definition.Foreach.With, contract.Scope{Args: node.Inputs, IterationItem: control.CurrentElement, IterationIndex: index})
	case "loop":
		index := control.Iteration
		child.IterationIndex = &index
		child.Address = node.ID + "/iteration/" + strconv.Itoa(index)
		child.GraphPath = graph.GraphPath + "/nodes/" + node.NodeID + "/body"
		child.Inputs, err = execution.BindWith(definition.Loop.With, contract.Scope{Args: node.Inputs, State: control.Carry, IterationIndex: index})
	}
	child.ID = execution.StableID("g", child.Address)
	return child, scope, err
}

func controlValue(value any) contract.Value {
	b, _ := json.Marshal(value)
	return contract.Value{JSON: b}
}

func failedControl(code, message string) *execution.ExecuteResult {
	return &execution.ExecuteResult{Failure: failure(code, message)}
}
