// Package executiontest holds the backend-neutral v1 compatibility scenarios.
package executiontest

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
)

type Action struct {
	At         time.Duration
	Human      *execution.HumanSignal
	Resolution *execution.ResolutionSignal
	Cancel     *execution.CancelSignal
}

type Scenario struct {
	Name          string
	Plan          contract.Plan
	Inputs        contract.Values
	Start         time.Time
	Execute       func(execution.ExecuteRequest) execution.ExecuteResult
	Actions       []Action
	WantStatus    string
	WantFailure   string
	WantOutputs   contract.Values
	WantCalls     map[string]int
	WantRootNodes map[string]string
	CheckCalls    func(*testing.T, []execution.ExecuteRequest)
}

type Observation struct {
	Result   execution.RunResult
	Calls    []execution.ExecuteRequest
	Requests []execution.Request
	// Terminal node observations are keyed by stable instance identity. The
	// suite makes no assertion about order of independent branch completions.
	TerminalNodes map[string]string
}

type Runner func(*testing.T, Scenario) Observation

func Run(t *testing.T, runner Runner) {
	t.Helper()
	for _, scenario := range Scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			got := runner(t, scenario)
			if got.Result.Status != scenario.WantStatus {
				t.Fatalf("status=%s want=%s failure=%v", got.Result.Status, scenario.WantStatus, got.Result.Failure)
			}
			code := ""
			if got.Result.Failure != nil {
				code = got.Result.Failure.Code
			}
			if code != scenario.WantFailure {
				t.Fatalf("failure=%s want=%s", code, scenario.WantFailure)
			}
			assertValues(t, got.Result.Outputs, scenario.WantOutputs)
			counts := map[string]int{}
			for _, call := range got.Calls {
				counts[call.NodeID]++
			}
			if !reflect.DeepEqual(counts, scenario.WantCalls) {
				t.Fatalf("leaf invocation counts=%v want=%v", counts, scenario.WantCalls)
			}
			for name, status := range scenario.WantRootNodes {
				id := execution.StableID("n", "test/root/"+name)
				if got.TerminalNodes[id] != status {
					t.Fatalf("node %s=%s want=%s", name, got.TerminalNodes[id], status)
				}
			}
			if scenario.CheckCalls != nil {
				scenario.CheckCalls(t, got.Calls)
			}
		})
	}
}

func assertValues(t *testing.T, got, want contract.Values) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("export count=%d want=%d", len(got), len(want))
	}
	for name, expected := range want {
		actual, present := got[name]
		if !present {
			t.Fatalf("export %s is absent", name)
		}
		if !reflect.DeepEqual(actual.Artifacts, expected.Artifacts) || actual.Collection != expected.Collection {
			t.Fatalf("artifact export %s changed", name)
		}
		var a, b any
		if len(actual.JSON) > 0 {
			if err := json.Unmarshal(actual.JSON, &a); err != nil {
				t.Fatal(err)
			}
		}
		if len(expected.JSON) > 0 {
			if err := json.Unmarshal(expected.JSON, &b); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(a, b) || (len(actual.JSON) == 0) != (len(expected.JSON) == 0) {
			t.Fatalf("export %s=%s want=%s", name, actual.JSON, expected.JSON)
		}
	}
}

func value(v any) contract.Value { data, _ := json.Marshal(v); return contract.Value{JSON: data} }
func literal(v any) *contract.Binding {
	data, _ := json.Marshal(v)
	return &contract.Binding{Value: data}
}
func from(source string) *contract.Binding { return &contract.Binding{From: source} }
func port(schema string, binding *contract.Binding) contract.Port {
	return contract.Port{Schema: json.RawMessage(schema), Bind: binding}
}

const stringSchema = `{"type":"string"}`
const integerSchema = `{"type":"integer"}`

func leaf(kind string) contract.Node {
	n := contract.Node{Type: kind, Outputs: map[string]contract.Port{"value": port(stringSchema, nil)}}
	switch kind {
	case "llm":
		n.LLM = &contract.LLMNode{Model: "fixture"}
	case "agent":
		n.Agent = &contract.AgentNode{Model: "fixture"}
	case "code":
		n.Code = &contract.CodeNode{Command: []string{"fixture"}}
	case "tool":
		n.Tool = &contract.ToolNode{Server: "fixture", Name: "fixture", Arguments: *literal(map[string]any{})}
	}
	return n
}

func graph(nodes map[string]contract.Node, source, schema string) contract.Graph {
	return contract.Graph{Nodes: nodes, Outputs: map[string]contract.Port{"result": port(schema, from(source))}}
}

func plan(g contract.Graph) contract.Plan {
	return contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "pipeline.yaml",
		Pipelines: map[string]*contract.Pipeline{"pipeline.yaml": {Spec: contract.Spec{Graph: g}}},
		Profile:   contract.Profile{Spec: contract.ProfileSpec{Limits: contract.Limits{Timeout: "1h", MaxConcurrentNodes: 8, MaxNodeInstances: 100, MaxModelCalls: 100, MaxToolCalls: 100}}},
	}
}

func success(name string, p contract.Plan, outputs contract.Values, calls map[string]int) Scenario {
	return Scenario{Name: name, Plan: p, Start: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), WantStatus: "succeeded", WantOutputs: outputs, WantCalls: calls,
		Execute: func(execution.ExecuteRequest) execution.ExecuteResult {
			return execution.ExecuteResult{Outputs: contract.Values{"value": value("ok")}}
		},
	}
}

// Scenarios returns fresh fixtures, including all nine node types. Additional
// crash, publication and external-operation tests need real infrastructure;
// passing this suite alone cannot establish backend readiness.
func Scenarios() []Scenario {
	var cases []Scenario
	for _, kind := range []string{"llm", "agent", "code", "tool"} {
		cases = append(cases, success(kind, plan(graph(map[string]contract.Node{"work": leaf(kind)}, "nodes.work.outputs.value", stringSchema)), contract.Values{"result": value("ok")}, map[string]int{"work": 1}))
	}
	cases = append(cases, success("empty_graph", plan(contract.Graph{Nodes: map[string]contract.Node{}}), nil, map[string]int{}))
	choose := contract.Node{Type: "switch", Switch: &contract.SwitchNode{Cases: []contract.SwitchCase{{Name: "left", When: "true"}, {Name: "right", When: "true"}}, Default: "right"}, Outputs: map[string]contract.Port{"route": port(stringSchema, nil)}}
	left, right := leaf("llm"), leaf("llm")
	for _, n := range []*contract.Node{&left, &right} {
		n.Dependencies = []string{"choose"}
		n.Inputs = map[string]contract.Port{"route": port(stringSchema, from("nodes.choose.outputs.route"))}
	}
	left.When = `args.route == "left"`
	right.When = `args.route == "right"`
	blocked := leaf("llm")
	blocked.Dependencies = []string{"right"}
	blocked.Needs = []string{"right"}
	g := contract.Graph{Nodes: map[string]contract.Node{"choose": choose, "left": left, "right": right, "blocked": blocked}, Outputs: map[string]contract.Port{"result": port(stringSchema, &contract.Binding{Coalesce: []contract.Binding{*from("nodes.right.outputs.value"), *from("nodes.left.outputs.value")}})}}
	branch := success("ordered_switch_coalesce_and_needs", plan(g), contract.Values{"result": value("ok")}, map[string]int{"left": 1})
	branch.WantRootNodes = map[string]string{"choose": "succeeded", "left": "succeeded", "right": "skipped", "blocked": "skipped"}
	cases = append(cases, branch)
	for _, required := range []bool{false, true} {
		n := leaf("llm")
		n.When = "false"
		g := graph(map[string]contract.Node{"work": n}, "nodes.work.outputs.value", stringSchema)
		p := g.Outputs["result"]
		p.Required = &required
		g.Outputs["result"] = p
		name := "optional_absence"
		if required {
			name = "required_absence"
		}
		s := success(name, plan(g), nil, map[string]int{})
		if required {
			s.WantStatus = "failed"
			s.WantFailure = "OUTPUT_UNAVAILABLE"
		}
		cases = append(cases, s)
	}
	first, next := leaf("llm"), leaf("llm")
	next.Dependencies = []string{"first"}
	next.Needs = []string{"first"}
	s := success("invalid_output_stops_dependency", plan(graph(map[string]contract.Node{"first": first, "next": next}, "nodes.next.outputs.value", stringSchema)), nil, map[string]int{"first": 1})
	s.Execute = func(execution.ExecuteRequest) execution.ExecuteResult {
		return execution.ExecuteResult{Outputs: contract.Values{"value": value(123)}}
	}
	s.WantStatus = "failed"
	s.WantFailure = "OUTPUT_INVALID"
	cases = append(cases, s)
	for _, items := range [][]int{{3, 1, 2}, {}} {
		work := leaf("llm")
		work.Inputs = map[string]contract.Port{"item": port(integerSchema, from("inputs.item"))}
		work.Outputs = map[string]contract.Port{"value": port(integerSchema, nil)}
		body := graph(map[string]contract.Node{"work": work}, "nodes.work.outputs.value", integerSchema)
		body.Inputs = map[string]contract.Port{"item": port(integerSchema, nil)}
		n := contract.Node{Type: "foreach", Inputs: map[string]contract.Port{"items": port(`{"type":"array","items":{"type":"integer"}}`, literal(items))}, Outputs: map[string]contract.Port{"result": port(`{"type":"array","items":{"type":"integer"}}`, nil)}, Foreach: &contract.ForeachNode{Over: "items", Concurrency: 3, With: map[string]contract.Binding{"item": *from("iteration.item")}, Body: body}}
		calls := map[string]int{}
		name := "foreach_empty"
		if len(items) > 0 {
			calls["work"] = len(items)
			name = "foreach_order"
		}
		s := success(name, plan(graph(map[string]contract.Node{"map": n}, "nodes.map.outputs.result", `{"type":"array","items":{"type":"integer"}}`)), contract.Values{"result": value(items)}, calls)
		s.Execute = func(req execution.ExecuteRequest) execution.ExecuteResult {
			return execution.ExecuteResult{Outputs: contract.Values{"value": req.Inputs["item"]}}
		}
		cases = append(cases, s)
	}
	human := contract.Node{Type: "human", Human: &contract.HumanNode{}, Outputs: map[string]contract.Port{"approved": port(`{"type":"boolean"}`, nil)}}
	s = success("human_first_valid_response", plan(graph(map[string]contract.Node{"review": human}, "nodes.review.outputs.approved", `{"type":"boolean"}`)), contract.Values{"result": value(true)}, map[string]int{})
	requestID := execution.StableID("h", execution.StableID("n", "test/root/review")+"/human")
	s.Actions = []Action{
		{At: time.Second, Human: &execution.HumanSignal{RequestID: requestID, ResponseID: "invalid", Values: contract.Values{"approved": value("invalid")}}},
		{At: 2 * time.Second, Human: &execution.HumanSignal{RequestID: requestID, ResponseID: "accepted", Values: contract.Values{"approved": value(true)}}},
	}
	cases = append(cases, s)
	child := &contract.Pipeline{Spec: contract.Spec{Limits: contract.Limits{MaxModelCalls: 2}, Graph: graph(map[string]contract.Node{"work": leaf("llm")}, "nodes.work.outputs.value", stringSchema)}}
	n := contract.Node{Type: "pipeline", Pipeline: &contract.PipelineNode{File: "child.yaml", Permissions: contract.Permissions{Models: []string{"fixture"}}}, Outputs: map[string]contract.Port{"result": port(stringSchema, nil)}}
	p := plan(graph(map[string]contract.Node{"child": n}, "nodes.child.outputs.result", stringSchema))
	p.Pipelines["child.yaml"] = child
	s = success("nested_pipeline_scope", p, contract.Values{"result": value("ok")}, map[string]int{"work": 1})
	s.CheckCalls = func(t *testing.T, calls []execution.ExecuteRequest) {
		for _, call := range calls {
			if len(call.Scopes) != 2 || call.Scopes[1].Limits.MaxModelCalls != 2 || call.Permissions == nil || !reflect.DeepEqual(call.Permissions.Models, []string{"fixture"}) {
				t.Fatalf("scope or permission inheritance changed: %+v", call)
			}
		}
	}
	cases = append(cases, s)
	work := leaf("code")
	work.Inputs = map[string]contract.Port{"a": port(integerSchema, from("inputs.a")), "b": port(integerSchema, from("inputs.b"))}
	work.Outputs = map[string]contract.Port{"a": port(integerSchema, nil), "b": port(integerSchema, nil)}
	body := contract.Graph{Inputs: map[string]contract.Port{"a": port(integerSchema, nil), "b": port(integerSchema, nil)}, Nodes: map[string]contract.Node{"copy": work}, Outputs: map[string]contract.Port{"a": port(integerSchema, from("nodes.copy.outputs.a")), "b": port(integerSchema, from("nodes.copy.outputs.b"))}}
	a, b := port(integerSchema, nil), port(integerSchema, nil)
	a.Initial = literal(1)
	a.Next = from("state.b")
	b.Initial = literal(2)
	b.Next = from("state.a")
	n = contract.Node{Type: "loop", Loop: &contract.LoopNode{MaxIterations: 2, State: map[string]contract.Port{"a": a, "b": b}, With: map[string]contract.Binding{"a": *from("state.a"), "b": *from("state.b")}, Body: body, Until: "iteration.index == 1"}, Outputs: map[string]contract.Port{"a": port(integerSchema, nil), "b": port(integerSchema, nil), "iterations": port(integerSchema, nil), "termination": port(stringSchema, nil)}}
	g = contract.Graph{Nodes: map[string]contract.Node{"repeat": n}, Outputs: map[string]contract.Port{"a": port(integerSchema, from("nodes.repeat.outputs.a")), "b": port(integerSchema, from("nodes.repeat.outputs.b")), "iterations": port(integerSchema, from("nodes.repeat.outputs.iterations"))}}
	s = success("loop_simultaneous_carry", plan(g), contract.Values{"a": value(2), "b": value(1), "iterations": value(2)}, map[string]int{"copy": 2})
	s.Execute = func(req execution.ExecuteRequest) execution.ExecuteResult {
		return execution.ExecuteResult{Outputs: req.Inputs}
	}
	cases = append(cases, s)
	return append(cases, policyScenarios()...)
}

func policyScenarios() []Scenario {
	var cases []Scenario
	nullNode := leaf("llm")
	nullSchema := `{"type":["string","null"]}`
	nullNode.Outputs = map[string]contract.Port{"value": port(nullSchema, nil)}
	nullCase := success("json_null_is_present", plan(graph(map[string]contract.Node{"work": nullNode}, "nodes.work.outputs.value", nullSchema)), contract.Values{"result": value(nil)}, map[string]int{"work": 1})
	nullCase.Execute = func(execution.ExecuteRequest) execution.ExecuteResult {
		return execution.ExecuteResult{Outputs: contract.Values{"value": value(nil)}}
	}
	cases = append(cases, nullCase)
	for _, safe := range []bool{true, false} {
		n := leaf("llm")
		n.Execution.Retry = &contract.Retry{MaxAttempts: 2, Backoff: "1s"}
		name := "unsafe_failure_never_retries"
		if safe {
			name = "explicit_safe_retry"
		}
		s := success(name, plan(graph(map[string]contract.Node{"work": n}, "nodes.work.outputs.value", stringSchema)), contract.Values{"result": value("ok")}, map[string]int{"work": 2})
		s.Execute = func(req execution.ExecuteRequest) execution.ExecuteResult {
			if req.Attempt == 1 {
				return execution.ExecuteResult{Failure: &execution.Failure{Code: "TRANSPORT", Message: "failed before sending", Retryable: safe}}
			}
			return execution.ExecuteResult{Outputs: contract.Values{"value": value("ok")}}
		}
		if !safe {
			s.WantStatus = "failed"
			s.WantFailure = "TRANSPORT"
			s.WantOutputs = nil
			s.WantCalls = map[string]int{"work": 1}
		}
		s.CheckCalls = func(t *testing.T, calls []execution.ExecuteRequest) {
			for i, call := range calls {
				if call.Attempt != i+1 || !call.Deadline.Equal(calls[0].Deadline) {
					t.Fatalf("retry changed deadline or attempt numbering: %+v", calls)
				}
			}
		}
		cases = append(cases, s)
	}
	expiring := leaf("llm")
	expiring.Execution.Timeout = "1s"
	expiring.Execution.Retry = &contract.Retry{MaxAttempts: 2, Backoff: "2s"}
	expired := success("retry_never_extends_deadline", plan(graph(map[string]contract.Node{"work": expiring}, "nodes.work.outputs.value", stringSchema)), nil, map[string]int{"work": 1})
	expired.WantStatus = "failed"
	expired.WantFailure = "DEADLINE_EXCEEDED"
	expired.Execute = func(execution.ExecuteRequest) execution.ExecuteResult {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "TRANSPORT", Message: "safe failure", Retryable: true}}
	}
	cases = append(cases, expired)
	n := leaf("llm")
	s := success("unknown_completed_evidence", plan(graph(map[string]contract.Node{"work": n}, "nodes.work.outputs.value", stringSchema)), contract.Values{"result": value("confirmed")}, map[string]int{"work": 1})
	s.Execute = func(execution.ExecuteRequest) execution.ExecuteResult {
		return execution.ExecuteResult{Failure: &execution.Failure{Code: "OUTCOME_UNKNOWN", Unknown: true, OperationID: "operation"}}
	}
	s.Actions = []Action{{At: time.Second, Resolution: &execution.ResolutionSignal{InstanceID: execution.StableID("n", "test/root/work"), OperationID: "operation", ResponseID: "resolved", Decision: "completed", Evidence: "external record 42", Outputs: contract.Values{"value": value("confirmed")}}}}
	cases = append(cases, s)
	a, b := leaf("llm"), leaf("llm")
	a.When = "false"
	b.When = "false"
	p := plan(graph(map[string]contract.Node{"a": a, "b": b}, "nodes.a.outputs.value", stringSchema))
	p.Profile.Spec.Limits.MaxNodeInstances = 1
	s = success("materialization_budget_includes_skips", p, nil, map[string]int{})
	s.WantStatus = "failed"
	s.WantFailure = "LIMIT_EXCEEDED"
	cases = append(cases, s)
	for _, mode := range []string{"fail", "return_last"} {
		body := graph(map[string]contract.Node{"work": leaf("llm")}, "nodes.work.outputs.value", stringSchema)
		n := contract.Node{Type: "loop", Loop: &contract.LoopNode{MaxIterations: 2, OnLimit: mode, Body: body, Until: "false"}, Outputs: map[string]contract.Port{"result": port(stringSchema, nil), "iterations": port(integerSchema, nil), "termination": port(stringSchema, nil)}}
		s := success("loop_limit_"+mode, plan(graph(map[string]contract.Node{"repeat": n}, "nodes.repeat.outputs.termination", stringSchema)), contract.Values{"result": value("limit")}, map[string]int{"work": 2})
		if mode == "fail" {
			s.WantStatus = "failed"
			s.WantFailure = "LIMIT_EXCEEDED"
			s.WantOutputs = nil
		}
		cases = append(cases, s)
	}
	human := contract.Node{Type: "human", Human: &contract.HumanNode{}, Outputs: map[string]contract.Port{"approved": port(`{"type":"boolean"}`, nil)}}
	s = success("cancel_human_wait", plan(graph(map[string]contract.Node{"review": human}, "nodes.review.outputs.approved", `{"type":"boolean"}`)), nil, map[string]int{})
	s.WantStatus = "cancelled"
	s.WantFailure = "CANCELLED"
	s.Actions = []Action{{At: time.Second, Cancel: &execution.CancelSignal{Reason: "fixture cancellation"}}}
	cases = append(cases, s)
	return cases
}
