// Package scheduler makes bounded execution decisions without external I/O.
package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

// ErrUnsupportedControl rejects node kinds outside the frozen v1 contract.
var ErrUnsupportedControl = errors.New("unsupported scheduler node kind")

type Snapshot struct {
	Run                 execution.RunState
	Graph               execution.GraphRecord
	Nodes               []execution.NodeRecord
	Attempts            []execution.AttemptRecord
	Scopes              []execution.ScopeRecord
	Timers              []execution.TimerRecord
	Requests            []execution.RequestRecord
	Controls            []execution.ControlRecord
	Children            []execution.GraphRecord
	ForeachOutputs      map[string]contract.Values
	UnresolvedElsewhere bool
	FocusInstanceID     string

	// A non-nil selection contains the only existing nodes eligible to change.
	// Nodes also includes their complete dependency values and, on demand, exports.
	SelectedNodeIDs      []string
	ActiveNodesElsewhere bool
	GraphExportsLoaded   bool
}

// Changes contain only changed rows. Complete values are never taken from the
// bounded observation projections or rewritten as one growing run snapshot.
type Changes struct {
	Controls         []execution.ControlRecord
	Children         []execution.GraphRecord
	NewScopes        []execution.ScopeRecord
	Transitions      int
	Graph            *execution.GraphRecord
	Nodes            []execution.NodeRecord
	Attempts         []execution.AttemptRecord
	Timers           []execution.TimerRecord
	ReserveInstances int
	StartRun         bool
	StopCause        *execution.Failure
	RootResult       *execution.RunResult
	Requests         []execution.Request
	Pause            bool
	PauseCause       *execution.Failure
	Resume           bool
	More             bool

	// Load these complete exports and rerun the pure step before saving changes.
	ForeachCollections []string
	GraphExports       bool
}

func terminal(state string) bool {
	return state == "succeeded" || state == "skipped" || state == "failed" || state == "cancelled"
}

func failure(code, message string) *execution.Failure {
	return &execution.Failure{Code: code, Message: message}
}

func asFailure(err error) *execution.Failure {
	var f *execution.Failure
	if errors.As(err, &f) {
		return f
	}
	return failure("EXECUTION_FAILED", err.Error())
}

// Step uses the post-lock database timestamp supplied in Run.Now. maxTransitions
// bounds materialization and node changes, including skips and synchronous nodes.
func Step(plan contract.Plan, snapshot Snapshot, maxTransitions int) (Changes, error) {
	var changes Changes
	if maxTransitions < 1 {
		return changes, errors.New("scheduler step requires a positive transition bound")
	}
	run, graph := snapshot.Run, snapshot.Graph
	if run.Backend != execution.BackendRiver || run.SchedulerVersion != execution.SchedulerVersion || run.StateFormatVersion != execution.StateFormatVersion {
		return changes, errors.New("unsupported scheduler execution version")
	}
	if terminal(graph.State) {
		return changes, nil
	}
	definition, err := execution.GraphDefinition(plan, graph.Pipeline, graph.GraphPath)
	if err != nil {
		return changes, err
	}
	pipeline := plan.Pipelines[graph.Pipeline]
	ids := make([]string, 0, len(definition.Nodes))
	for id, node := range definition.Nodes {
		ids = append(ids, id)
		if node.Type != "llm" && node.Type != "agent" && node.Type != "code" && node.Type != "tool" && node.Type != "switch" && node.Type != "human" && !composite(node.Type) {
			return changes, fmt.Errorf("%w: %s (%s)", ErrUnsupportedControl, id, node.Type)
		}
	}
	slices.Sort(ids)
	stepIDs := slices.Clone(ids)
	if snapshot.SelectedNodeIDs != nil {
		stepIDs = slices.Clone(snapshot.SelectedNodeIDs)
		slices.Sort(stepIDs)
	}
	nodes := make(map[string]execution.NodeRecord, len(snapshot.Nodes))
	values := make(map[string]contract.Values, len(snapshot.Nodes))
	attempts := make(map[execution.AttemptID]execution.AttemptRecord, len(snapshot.Attempts))
	for _, node := range snapshot.Nodes {
		if node.RunID != run.RunID || node.GraphID != graph.ID {
			return changes, errors.New("node does not belong to the selected graph")
		}
		nodes[node.NodeID] = node
		if terminal(node.State) {
			values[node.NodeID] = node.Outputs
		}
	}
	for _, attempt := range snapshot.Attempts {
		attempts[attempt.AttemptID] = attempt
	}
	requests := make(map[string]execution.RequestRecord, len(snapshot.Requests))
	for _, request := range snapshot.Requests {
		requests[request.ID] = request
	}
	controls := make(map[string]execution.ControlRecord, len(snapshot.Controls))
	for _, control := range snapshot.Controls {
		controls[control.InstanceID] = control
	}
	remaining := maxTransitions
	graphChanged := false
	cause := run.StopCause
	if cause == nil && run.Cancelled {
		cause = failure("CANCELLED", "requested by operator")
	} else if cause == nil && (!graph.Deadline.After(run.Now) || !run.Deadline.After(run.Now)) {
		cause = failure("DEADLINE_EXCEEDED", "execution deadline exceeded")
	}
	if graph.State == "pending" && cause == nil {
		inputs, err := contract.ValidatePorts(definition.Inputs, graph.Inputs, true)
		if err != nil {
			cause = failure("INPUT_INVALID", err.Error())
		}
		for _, scope := range snapshot.Scopes {
			if scope.Limits.MaxNodeInstances <= 0 || scope.MaterializedInstances+len(ids) > scope.Limits.MaxNodeInstances {
				cause = failure("LIMIT_EXCEEDED", "node instance budget exhausted in "+scope.ID)
				break
			}
		}
		if cause == nil {
			graph.Inputs, graph.State = inputs, "running"
			changes.ReserveInstances = len(ids)
			changes.StartRun = graph.ParentInstanceID == "" && run.Status == "pending"
			graphChanged = true
		}
	}
	appendNode := func(node execution.NodeRecord, state string, outputs contract.Values, f *execution.Failure, reason string) {
		if terminal(state) && state != "succeeded" {
			request := requests[execution.NodeRequestID(node)]
			if request.Status == "open" || request.Status == "pending" {
				closed := request.Request
				closed.Status = "cancelled"
				if !node.Deadline.After(run.Now) {
					closed.Status = "expired"
				}
				changes.Requests = append(changes.Requests, closed)
			}
		}
		node.State, node.Outputs, node.Failure, node.Reason = state, outputs, f, reason
		node.Revision++
		if node.StartedAt == nil && (state == "running" || state == "waiting_human") {
			now := run.Now
			node.StartedAt = &now
		}
		if terminal(state) {
			now := run.Now
			node.FinishedAt = &now
			values[node.NodeID] = outputs
		}
		nodes[node.NodeID] = node
		changes.Nodes = append(changes.Nodes, node)
		remaining--
	}
	if cause == nil {
		for graph.MaterializationCursor < len(ids) && remaining > 0 {
			id := ids[graph.MaterializationCursor]
			address := graph.Address + "/" + id
			node := execution.NodeRecord{RunID: run.RunID, ID: execution.StableID("n", address), GraphID: graph.ID, NodeID: id, Address: address}
			appendNode(node, "pending", nil, nil, "waiting for dependencies")
			if snapshot.SelectedNodeIDs != nil {
				stepIDs = append(stepIDs, id)
			}
			graph.MaterializationCursor++
			graphChanged = true
		}
		if graph.MaterializationCursor < len(ids) {
			changes.More = true
		}
	}
	// Finish admitted work before admitting its downstream nodes. A terminal
	// result's complete values are part of this same proposed transaction.
	slices.Sort(stepIDs)
	finishIDs := slices.Clone(stepIDs)
	// Accepted commands get their eligible transition within this bounded step,
	// before unrelated completions can consume its transition allowance.
	slices.SortStableFunc(finishIDs, func(a, b string) int {
		if nodes[a].ID != nodes[b].ID {
			if nodes[a].ID == snapshot.FocusInstanceID {
				return -1
			}
			if nodes[b].ID == snapshot.FocusInstanceID {
				return 1
			}
		}
		return responsePriority(nodes[a], requests) - responsePriority(nodes[b], requests)
	})
	for _, id := range finishIDs {
		if remaining == 0 {
			break
		}
		node, found := nodes[id]
		decision, closed := requestDecision(node, requests)
		if cause != nil && (decision == nil || run.StopCause != nil || run.Cancelled || cause.Code != "DEADLINE_EXCEEDED") {
			continue
		}
		if decision == nil && found && !terminal(node.State) && !node.Deadline.IsZero() && !node.Deadline.After(run.Now) {
			cause = failure("DEADLINE_EXCEEDED", "node deadline exceeded")
			appendNode(node, "failed", nil, cause, "")
			break
		}
		if decision == nil && (!found || node.State != "running" || node.Request == nil) {
			if found && node.State == "retry_wait" && node.Request != nil && !run.Paused {
				for _, timer := range snapshot.Timers {
					if timer.InstanceID != node.ID || timer.Generation != int64(node.AttemptNumber) || timer.DueAt.After(run.Now) {
						continue
					}
					node.AttemptNumber++
					request := *node.Request
					request.Attempt = node.AttemptNumber
					node.Request = &request
					changes.Attempts = append(changes.Attempts, execution.AttemptRecord{AttemptID: execution.AttemptID{RunID: run.RunID, InstanceID: node.ID, Number: node.AttemptNumber},
						State: "ready", DispatchGeneration: 1, DispatchPending: true})
					appendNode(node, "ready", nil, nil, "waiting for execution capacity")
					break
				}
			}
			continue
		}
		var result execution.ExecuteResult
		switch {
		case decision != nil:
			result = *decision
			changes.Requests = append(changes.Requests, *closed)
		case composite(node.Request.Node.Type):
			controlResult, advanced := advanceControl(plan, run, graph, node, controls[node.ID], snapshot.Children, snapshot.ForeachOutputs, &changes)
			if advanced {
				remaining--
			}
			if controlResult == nil {
				continue
			}
			result = *controlResult
		case node.Request.Node.Type == "switch":
			outputs, err := execution.RunSwitch(node.Request.Node, node.Inputs)
			if err != nil {
				result.Failure = asFailure(err)
			} else {
				result.Outputs = outputs
			}
		default:
			attempt := attempts[execution.AttemptID{RunID: run.RunID, InstanceID: node.ID, Number: node.AttemptNumber}]
			if attempt.State != "completed" || attempt.Result == nil {
				continue
			}
			result = *attempt.Result

		}
		if result.Failure == nil {
			outputs, err := contract.ValidatePorts(node.Request.Node.Outputs, result.Outputs, false)
			if err == nil {
				appendNode(node, "succeeded", execution.WithoutArtifactPaths(outputs), nil, "")
				continue
			}
			result.Failure = failure("OUTPUT_INVALID", err.Error())
		}
		if result.Failure.Unknown && node.Request.Node.Execution.OnUnknownOutcome != "fail" {
			f := *result.Failure
			if f.OperationID == "" {
				f.OperationID = fmt.Sprintf("%s/attempt/%d", node.ID, node.AttemptNumber)
			}
			appendNode(node, "waiting_resolution", nil, &f, "external outcome requires evidence")
			changes.Pause = true
			changes.PauseCause = &f
			changes.Requests = append(changes.Requests, execution.Request{RunID: run.RunID,
				ID: execution.StableID("q", node.ID+"/resolution/"+fmt.Sprint(node.AttemptNumber)), InstanceID: node.ID,
				Kind: "resolution", Status: "open", Failure: &f, Outputs: node.Request.Node.Outputs, Deadline: node.Deadline})
			continue
		}
		if !composite(node.Request.Node.Type) && !result.Failure.Unknown && result.Failure.Retryable && node.AttemptNumber < node.Request.Node.Execution.Retry.MaxAttempts {
			backoff, err := contract.Duration(node.Request.Node.Execution.Retry.Backoff)
			if err != nil || backoff < 0 {
				return Changes{}, errors.New("invalid admitted retry backoff")
			}
			changes.Timers = append(changes.Timers, execution.TimerRecord{RunID: run.RunID, ID: "retry/" + node.ID, InstanceID: node.ID,
				Generation: int64(node.AttemptNumber), Kind: "retry", DueAt: run.Now.Add(backoff)})
			appendNode(node, "retry_wait", nil, result.Failure, "waiting for retry backoff")
			continue
		}
		appendNode(node, "failed", nil, result.Failure, "")
		if cause == nil {
			cause = result.Failure
		}
	}
	if run.Paused && cause == nil && !changes.Pause && !snapshot.UnresolvedElsewhere {
		changes.Resume = true
		for _, node := range nodes {
			if node.State == "waiting_resolution" {
				changes.Resume = false
				break
			}
		}
		changes.More = changes.More || changes.Resume
	}
	if cause == nil && graph.MaterializationCursor == len(ids) && (!run.Paused || changes.Resume) && !changes.Pause {
		for _, id := range stepIDs {
			if remaining == 0 {
				break
			}
			node := nodes[id]
			if node.State != "pending" {
				continue
			}
			definitionNode := definition.Nodes[id]
			ready := true
			for _, dependency := range definitionNode.Dependencies {
				if _, found := definition.Nodes[dependency]; !found {
					cause = failure("PLAN_INVALID", "unknown dependency "+dependency)
					break
				}
				if !terminal(nodes[dependency].State) {
					ready = false
				}
			}
			if cause != nil {
				break
			}
			if !ready {
				continue
			}
			state, request, f, reason := admit(run, graph, pipeline.Spec.Defaults.Execution, definitionNode, node, nodes, values)
			node.Request = request
			if request != nil {
				node.Inputs, node.Deadline = request.Inputs, request.Deadline
			}
			if state == "ready" {
				node.AttemptNumber = 1
				changes.Attempts = append(changes.Attempts, execution.AttemptRecord{AttemptID: execution.AttemptID{RunID: run.RunID, InstanceID: node.ID, Number: 1},
					State: "ready", DispatchGeneration: 1, DispatchPending: true})
			}
			if state == "waiting_human" {
				node.AttemptNumber = 1
				changes.Requests = append(changes.Requests, execution.Request{RunID: run.RunID, ID: execution.StableID("h", node.ID+"/human"),
					InstanceID: node.ID, Kind: "human", Status: "open", Prompt: request.Node.Human.Prompt.Text,
					Inputs: execution.WithoutArtifactPaths(node.Inputs), Outputs: request.Node.Outputs, Deadline: node.Deadline})
			}
			appendNode(node, state, nil, f, reason)
			if f != nil {
				cause = f
				break
			}
			if state == "running" {
				changes.More = true
			}
		}
	}
	if cause != nil {
		changes.StopCause = cause
		for _, id := range stepIDs {
			if remaining == 0 {
				break
			}
			node, found := nodes[id]
			if found && !terminal(node.State) {
				appendNode(node, "cancelled", nil, nil, "parent stopped")
			}
		}
	}
	complete := (graph.MaterializationCursor == len(ids) || cause != nil) && !snapshot.ActiveNodesElsewhere
	for _, node := range nodes {
		if !terminal(node.State) {
			complete = false
			break
		}
	}
	if complete {
		if cause == nil && snapshot.SelectedNodeIDs != nil && !snapshot.GraphExportsLoaded {
			changes.GraphExports = true
			return changes, nil
		}
		if cause == nil {
			outputs, err := execution.BindPorts(definition.Outputs, contract.Scope{Inputs: graph.Inputs, Nodes: values}, false)
			if err != nil {
				cause = asFailure(err)
			} else {
				graph.Outputs, graph.State = outputs, "succeeded"
			}
		}
		if cause != nil {
			graph.Failure, graph.State = cause, "failed"
			if cause.Code == "CANCELLED" {
				graph.State = "cancelled"
			}
			changes.StopCause = cause
		}
		graphChanged = true
		changes.More = false
		if graph.ParentInstanceID == "" {
			changes.RootResult = &execution.RunResult{Status: graph.State, Outputs: graph.Outputs, Failure: graph.Failure}
		}
	} else if remaining == 0 || cause != nil {
		changes.More = true
	}
	if graphChanged {
		graph.Revision++
		changes.Graph = &graph
	}
	changes.Transitions = maxTransitions - remaining
	return changes, nil
}

func admit(run execution.RunState, graph execution.GraphRecord, defaults contract.Execution, definition contract.Node, node execution.NodeRecord,
	siblings map[string]execution.NodeRecord, values map[string]contract.Values) (string, *execution.ExecuteRequest, *execution.Failure, string) {
	for _, dependency := range definition.Needs {
		if siblings[dependency].State != "succeeded" {
			return "skipped", nil, nil, "needs dependency did not succeed"
		}
	}
	args, err := execution.BindPorts(definition.Inputs, contract.Scope{Inputs: graph.Inputs, Nodes: values}, true)
	if err != nil {
		return "failed", nil, asFailure(err), ""
	}
	if args == nil {
		return "skipped", nil, nil, "required input is missing"
	}
	if definition.When != "" {
		enabled, present, err := contract.EvalBool(definition.When, contract.Scope{Inputs: graph.Inputs, Nodes: values, Args: args})
		if err != nil {
			return "failed", nil, failure("CONDITION_INVALID", err.Error()), ""
		}
		if !present {
			return "skipped", nil, nil, "condition dependency is missing"
		}
		if !enabled {
			return "skipped", nil, nil, "condition is false"
		}
	}
	definition.Execution = execution.ExecutionPolicy(defaults, definition.Execution)
	deadline := node.Deadline
	if deadline.IsZero() {
		deadline = execution.NodeDeadline(run.Now, graph.Deadline, definition)
	}
	if !deadline.After(run.Now) {
		return "failed", nil, failure("DEADLINE_EXCEEDED", "node deadline exceeded"), ""
	}
	request := &execution.ExecuteRequest{RunID: run.RunID, InstanceID: node.ID, NodeID: node.NodeID, Pipeline: graph.Pipeline, Attempt: 1,
		Node: definition, Inputs: args, Scopes: graph.Scopes, Permissions: graph.Permissions, Deadline: deadline}
	if definition.Type == "tool" {
		if definition.Tool == nil {
			return "failed", request, failure("PLAN_INVALID", "missing tool configuration"), ""
		}
		value, present, err := contract.EvalBinding(definition.Tool.Arguments, contract.Scope{Args: args})
		var object map[string]json.RawMessage
		if err != nil || !present || value.Artifacts != nil || value.Collection || json.Unmarshal(value.JSON, &object) != nil || object == nil {
			return "failed", request, failure("INPUT_INVALID", "tool arguments must be a JSON object"), ""
		}
		request.ToolArguments = value.JSON
	}
	if definition.Type == "switch" || composite(definition.Type) {
		return "running", request, nil, ""
	}
	if definition.Type == "human" {
		if definition.Human == nil {
			return "failed", request, failure("PLAN_INVALID", "missing human configuration"), ""
		}
		return "waiting_human", request, nil, "waiting for a human response"
	}
	return "ready", request, nil, "waiting for execution capacity"
}
