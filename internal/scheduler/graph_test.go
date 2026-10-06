package scheduler

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/execution/executiontest"
)

func TestImplementedCompatibilityAtBoundedStepSizes(t *testing.T) {
	for _, bound := range []int{1, 2, 64} {
		t.Run(strconv.Itoa(bound), func(t *testing.T) {
			executiontest.Run(t, func(t *testing.T, scenario executiontest.Scenario) executiontest.Observation {
				limits := scenario.Plan.Profile.Spec.Limits
				deadline := scenario.Start.Add(time.Hour)
				snapshot := Snapshot{Run: execution.RunState{RunID: "test", Backend: execution.BackendRiver, SchedulerVersion: execution.SchedulerVersion,
					StateFormatVersion: execution.StateFormatVersion, Status: "pending", Now: scenario.Start, Deadline: deadline},
					Graph:  execution.RootGraph("test", scenario.Plan.Root, scenario.Inputs, limits, deadline),
					Scopes: []execution.ScopeRecord{{RunID: "test", ID: "test", Limits: limits}}}
				rootID := snapshot.Graph.ID
				graphs := map[string]execution.GraphRecord{rootID: snapshot.Graph}
				scopes := map[string]execution.ScopeRecord{"test": snapshot.Scopes[0]}
				controls := map[string]execution.ControlRecord{}
				nodes := map[string]execution.NodeRecord{}
				attempts := map[execution.AttemptID]execution.AttemptRecord{}
				requests := map[string]execution.RequestRecord{}
				nextAction := 0
				got := executiontest.Observation{TerminalNodes: map[string]string{}}
				for range 500 {
					for nextAction < len(scenario.Actions) && !scenario.Start.Add(scenario.Actions[nextAction].At).After(snapshot.Run.Now) {
						action := scenario.Actions[nextAction]
						nextAction++
						if action.Cancel != nil {
							snapshot.Run.Cancelled = true
						}
						for id, request := range requests {
							if request.Status != "open" || !request.Deadline.After(snapshot.Run.Now) {
								continue
							}
							if action.Human != nil && action.Human.RequestID == id {
								values, err := contract.ValidatePorts(request.Outputs, action.Human.Values, false)
								if err == nil {
									request.Answer, _ = json.Marshal(values)
									request.Status, request.ResponseID, request.AcceptedAt = "answered", action.Human.ResponseID, snapshot.Run.Now
								}
							}
							if action.Resolution != nil && action.Resolution.InstanceID == request.InstanceID {
								request.Answer, _ = json.Marshal(action.Resolution)
								request.Status, request.ResponseID, request.AcceptedAt = "resolved", action.Resolution.ResponseID, snapshot.Run.Now
							}
							requests[id] = request
						}
					}
					graphIDs := make([]string, 0, len(graphs))
					for id := range graphs {
						graphIDs = append(graphIDs, id)
					}
					slices.Sort(graphIDs)
					progress := false
					for _, graphID := range graphIDs {
						snapshot.Graph = graphs[graphID]
						if terminal(snapshot.Graph.State) {
							continue
						}
						snapshot.Nodes, snapshot.Attempts = nil, nil
						snapshot.Controls, snapshot.Children, snapshot.Scopes = nil, nil, nil
						snapshot.UnresolvedElsewhere = false
						for _, scope := range snapshot.Graph.Scopes {
							snapshot.Scopes = append(snapshot.Scopes, scopes[scope.ID])
						}
						for _, control := range controls {
							if nodes[control.InstanceID].GraphID == graphID {
								snapshot.Controls = append(snapshot.Controls, control)
							}
						}
						for _, child := range graphs {
							if nodes[child.ParentInstanceID].GraphID == graphID {
								snapshot.Children = append(snapshot.Children, child)
							}
						}
						snapshot.ForeachOutputs = map[string]contract.Values{}
						for i, control := range snapshot.Controls {
							control.ActiveChildren, control.SucceededChildren, control.FailedChildren = 0, 0, 0
							for _, child := range snapshot.Children {
								if child.ParentInstanceID != control.InstanceID {
									continue
								}
								switch child.State {
								case "succeeded":
									control.SucceededChildren++
								case "failed", "cancelled":
									control.FailedChildren++
								default:
									control.ActiveChildren++
								}
							}
							if control.Kind == "foreach" && control.NextPosition < control.ElementCount {
								item := control.Elements[control.NextPosition]
								control.CurrentElement = &item
							} else {
								control.CurrentElement = nil
							}
							snapshot.Controls[i] = control
							if control.Kind == "foreach" && control.CollectionPosition == control.ElementCount && control.SucceededChildren == control.ElementCount && control.ActiveChildren == 0 && control.FailedChildren == 0 {
								outputs, err := collectForeach(nodes[control.InstanceID].Request.Node.Foreach.Body, control.InstanceID, control.Elements, snapshot.Children)
								if err != nil {
									t.Fatal(err)
								}
								snapshot.ForeachOutputs[control.InstanceID] = outputs
							}
						}
						snapshot.Children = slices.DeleteFunc(snapshot.Children, func(child execution.GraphRecord) bool {
							for _, control := range snapshot.Controls {
								if control.Kind == "foreach" && control.InstanceID == child.ParentInstanceID && child.IterationIndex != nil && *child.IterationIndex < control.CollectionPosition {
									return true
								}
							}
							return false
						})
						slices.SortFunc(snapshot.Children, func(a, b execution.GraphRecord) int {
							if a.ParentInstanceID != b.ParentInstanceID {
								return cmp.Compare(a.ParentInstanceID, b.ParentInstanceID)
							}
							if a.IterationIndex != nil && b.IterationIndex != nil {
								return cmp.Compare(*a.IterationIndex, *b.IterationIndex)
							}
							return cmp.Compare(a.ID, b.ID)
						})
						snapshot.Requests = nil
						for _, request := range requests {
							if nodes[request.InstanceID].GraphID == graphID {
								snapshot.Requests = append(snapshot.Requests, request)
							}
						}
						for _, node := range nodes {
							if node.GraphID == graphID {
								snapshot.Nodes = append(snapshot.Nodes, node)
							} else if node.State == "waiting_resolution" {
								snapshot.UnresolvedElsewhere = true
							}
						}
						for _, attempt := range attempts {
							if nodes[attempt.InstanceID].GraphID == graphID {
								snapshot.Attempts = append(snapshot.Attempts, attempt)
							}
						}
						changes, err := Step(scenario.Plan, snapshot, bound)
						if err != nil {
							t.Fatal(err)
						}
						if changes.Transitions > bound {
							t.Fatalf("transition bound exceeded: %d > %d", changes.Transitions, bound)
						}
						if changes.Graph != nil {
							graphs[graphID] = *changes.Graph
						}
						if changes.StartRun {
							snapshot.Run.Status = "running"
						}
						if changes.StopCause != nil {
							snapshot.Run.StopCause = changes.StopCause
						}
						if changes.Pause {
							snapshot.Run.Paused, snapshot.Run.Status = true, "waiting_resolution"
						}
						if changes.Resume {
							snapshot.Run.Paused, snapshot.Run.Status = false, "running"
						}
						for _, request := range changes.Requests {
							record := requests[request.ID]
							record.Request = request
							requests[request.ID] = record
							got.Requests = append(got.Requests, request)
						}
						for _, scope := range changes.NewScopes {
							scopes[scope.ID] = scope
						}
						for _, scope := range snapshot.Scopes {
							record := scopes[scope.ID]
							record.MaterializedInstances += changes.ReserveInstances
							scopes[scope.ID] = record
						}
						for _, control := range changes.Controls {
							controls[control.InstanceID] = control
						}
						for _, child := range changes.Children {
							graphs[child.ID] = child
						}
						snapshot.Timers = append(snapshot.Timers, changes.Timers...)
						for _, node := range changes.Nodes {
							nodes[node.ID] = node
							if terminal(node.State) {
								got.TerminalNodes[node.ID] = node.State
							}
						}
						for _, attempt := range changes.Attempts {
							attempts[attempt.AttemptID] = attempt
						}
						progress = progress || changes.Transitions > 0 || changes.Graph != nil || changes.More
					}
					root := graphs[rootID]
					done := terminal(root.State)
					for _, graph := range graphs {
						done = done && terminal(graph.State)
					}
					if done {
						got.Result = execution.RunResult{Status: root.State, Outputs: root.Outputs, Failure: root.Failure}
						return got
					}
					for id, attempt := range attempts {
						if attempt.State != "ready" {
							continue
						}
						node := nodes[id.InstanceID]
						if node.State != "ready" || snapshot.Run.Cancelled || snapshot.Run.StopCause != nil || snapshot.Run.Paused {
							continue
						}
						request := *node.Request
						progress = true
						got.Calls = append(got.Calls, request)
						result := scenario.Execute(request)
						attempt.State, attempt.Result = "completed", &result
						attempts[id] = attempt
						node.State, node.Revision = "running", node.Revision+1
						nodes[node.ID] = node
					}
					if !progress {
						next := snapshot.Run.Deadline
						if nextAction < len(scenario.Actions) {
							if at := scenario.Start.Add(scenario.Actions[nextAction].At); at.Before(next) {
								next = at
							}
						}
						for _, node := range nodes {
							if !terminal(node.State) && !node.Deadline.IsZero() && node.Deadline.Before(next) {
								next = node.Deadline
							}
							if node.State == "retry_wait" {
								if node.Deadline.Before(next) {
									next = node.Deadline
								}
								for _, timer := range snapshot.Timers {
									if timer.InstanceID == node.ID && timer.Generation == int64(node.AttemptNumber) && timer.DueAt.Before(next) {
										next = timer.DueAt
									}
								}
							}
						}
						snapshot.Run.Now = next
					}
				}
				t.Fatal("scheduler did not reach a terminal state")
				return got
			})
		})
	}
}

// Retained in-memory collection for the compatibility fixture; production uses paged SQL reads.
func collectForeach(body contract.Graph, parentID string, elements []contract.Value, children []execution.GraphRecord) (contract.Values, error) {
	results := make([]contract.Values, len(elements))
	seen := make([]bool, len(elements))
	for _, child := range children {
		if child.ParentInstanceID != parentID {
			continue
		}
		if child.IterationIndex == nil || *child.IterationIndex < 0 || *child.IterationIndex >= len(results) || child.State != "succeeded" || seen[*child.IterationIndex] {
			return nil, failure("PLAN_INVALID", "foreach child position is inconsistent")
		}
		results[*child.IterationIndex] = child.Outputs
		seen[*child.IterationIndex] = true
	}
	for _, present := range seen {
		if !present {
			return nil, failure("PLAN_INVALID", "admitted foreach child is missing")
		}
	}
	outputs := contract.Values{}
	for name, port := range body.Outputs {
		if port.Artifact != nil {
			value := contract.Value{Collection: true, Artifacts: make([]contract.Artifact, 0, len(results))}
			for _, result := range results {
				v, present := result[name]
				if !present || v.Collection || len(v.Artifacts) != 1 {
					return nil, failure("OUTPUT_INVALID", "foreach body must export a single artifact at "+name)
				}
				value.Artifacts = append(value.Artifacts, v.Artifacts[0])
			}
			outputs[name] = value
		} else {
			values := make([]json.RawMessage, 0, len(results))
			for _, result := range results {
				v, present := result[name]
				if !present {
					return nil, failure("OUTPUT_UNAVAILABLE", "foreach body output missing: "+name)
				}
				values = append(values, v.JSON)
			}
			outputs[name] = controlValue(values)
		}
	}
	return outputs, nil
}
