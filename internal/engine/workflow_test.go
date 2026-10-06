package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/michael-bill/knotra/internal/contract"
)

type harness struct {
	env        *testsuite.TestWorkflowEnvironment
	mu         sync.Mutex
	events     []Projection
	requests   []Request
	leaf       func(ExecuteRequest) ExecuteResult
	answer     *HumanSignal
	resolution *ResolutionSignal
	calls      []ExecuteRequest
	onProject  func(Projection)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	h := &harness{env: suite.NewTestWorkflowEnvironment()}
	h.env.RegisterWorkflow(Workflow)
	h.env.RegisterActivityWithOptions(func(_ context.Context, event Projection) error {
		h.mu.Lock()
		h.events = append(h.events, event)
		h.mu.Unlock()
		if h.onProject != nil {
			h.onProject(event)
		}
		return nil
	}, activity.RegisterOptions{Name: ProjectActivity})
	h.env.RegisterActivityWithOptions(func(_ context.Context, request Request) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, request)
		return nil
	}, activity.RegisterOptions{Name: RequestActivity})
	h.env.RegisterActivityWithOptions(
		func(_ context.Context, _ AnswerRequest) (*HumanSignal, error) { return h.answer, nil },
		activity.RegisterOptions{Name: AnswerActivity},
	)
	h.env.RegisterActivityWithOptions(
		func(_ context.Context, _ AnswerRequest) (*ResolutionSignal, error) { return h.resolution, nil },
		activity.RegisterOptions{Name: ResolutionActivity},
	)
	h.env.RegisterActivityWithOptions(func(_ context.Context, request ExecuteRequest) (ExecuteResult, error) {
		h.mu.Lock()
		h.calls = append(h.calls, request)
		h.mu.Unlock()
		if h.leaf != nil {
			return h.leaf(request), nil
		}
		return ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}}, nil
	}, activity.RegisterOptions{Name: ExecuteActivity})
	t.Cleanup(func() { h.env.AssertExpectations(t) })
	return h
}

func (h *harness) run(t *testing.T, plan contract.Plan, inputs contract.Values) RunResult {
	t.Helper()
	h.env.ExecuteWorkflow(Workflow, RunInput{RunID: "test", Plan: plan, Inputs: inputs})
	if err := h.env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := h.env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func plan(graph contract.Graph) contract.Plan {
	return contract.Plan{
		Version:         "knotra/v1",
		CompilerVersion: contract.CompilerVersion,
		CELVersion:      contract.CELVersion,
		Root:            "pipeline.yaml",
		Pipelines:       map[string]*contract.Pipeline{"pipeline.yaml": {Spec: contract.Spec{Graph: graph}}},
		Profile: contract.Profile{Spec: contract.ProfileSpec{Limits: contract.Limits{
			Timeout:            "1h",
			MaxConcurrentNodes: 8,
			MaxNodeInstances:   100,
			MaxModelCalls:      100,
			MaxToolCalls:       100,
		}}},
	}
}

func port(schema string, binding *contract.Binding) contract.Port {
	return contract.Port{Schema: json.RawMessage(schema), Bind: binding}
}

func from(source string) *contract.Binding { return &contract.Binding{From: source} }

func literal(value any) *contract.Binding {
	raw, _ := json.Marshal(value)
	return &contract.Binding{Value: raw}
}

func expression(value string) *contract.Binding { return &contract.Binding{Expr: value} }

func outputGraph(nodes map[string]contract.Node, source, schema string) contract.Graph {
	return contract.Graph{Nodes: nodes, Outputs: map[string]contract.Port{"result": port(schema, from(source))}}
}

func llm() contract.Node {
	return contract.Node{
		Type:    "llm",
		LLM:     &contract.LLMNode{Model: "model"},
		Outputs: map[string]contract.Port{"value": port(`{"type":"string"}`, nil)},
	}
}

func rootID(node string) string { return stableID("n", "test/root/"+node) }

func assertJSON(t *testing.T, value contract.Value, expected string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(value.JSON, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	gotText, _ := json.Marshal(got)
	wantText, _ := json.Marshal(want)
	if string(gotText) != string(wantText) {
		t.Fatalf("got %s, want %s", gotText, wantText)
	}
}

func TestBranchSkipAndOrderedCoalesce(t *testing.T) {
	h := newHarness(t)
	choose := contract.Node{
		Type:    "switch",
		Switch:  &contract.SwitchNode{Cases: []contract.SwitchCase{{Name: "left", When: "true"}}, Default: "right"},
		Outputs: map[string]contract.Port{"route": port(`{"type":"string"}`, nil)},
	}
	left, right := llm(), llm()

	for _, node := range []*contract.Node{&left, &right} {
		node.Dependencies = []string{"choose"}
		node.Inputs = map[string]contract.Port{"route": port(`{"type":"string"}`, from("nodes.choose.outputs.route"))}
	}

	left.When, right.When = `args.route == "left"`, `args.route == "right"`
	blocked := llm()
	blocked.Needs, blocked.Dependencies = []string{"right"}, []string{"right"}
	graph := contract.Graph{
		Nodes: map[string]contract.Node{"choose": choose, "left": left, "right": right, "blocked": blocked},
		Outputs: map[string]contract.Port{"result": port(
			`{"type":"string"}`,
			&contract.Binding{Coalesce: []contract.Binding{*from("nodes.right.outputs.value"), *from("nodes.left.outputs.value")}},
		)},
	}
	result := h.run(t, plan(graph), nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `"ok"`)
	if len(h.calls) != 1 || h.calls[0].NodeID != "left" {
		t.Fatalf("executed skipped branch: %+v", h.calls)
	}
}

func TestForeachPreservesInputOrder(t *testing.T) {
	h := newHarness(t)
	bodyLeaf := llm()
	bodyLeaf.Inputs = map[string]contract.Port{"item": port(`{"type":"integer"}`, from("inputs.item"))}
	bodyLeaf.Outputs = map[string]contract.Port{"value": port(`{"type":"integer"}`, nil)}
	body := outputGraph(map[string]contract.Node{"work": bodyLeaf}, "nodes.work.outputs.value", `{"type":"integer"}`)
	body.Inputs = map[string]contract.Port{"item": port(`{"type":"integer"}`, nil)}
	node := contract.Node{
		Type:    "foreach",
		Inputs:  map[string]contract.Port{"items": port(`{"type":"array","items":{"type":"integer"}}`, literal([]int{3, 1, 2}))},
		Outputs: map[string]contract.Port{"result": port(`{"type":"array","items":{"type":"integer"}}`, nil)},
		Foreach: &contract.ForeachNode{
			Over:        "items",
			Concurrency: 3,
			With:        map[string]contract.Binding{"item": *from("iteration.item")},
			Body:        body,
		},
	}

	for _, value := range []int{1, 2, 3} {
		v := value
		h.env.OnActivity(
			ExecuteActivity,
			mock.Anything,
			mock.MatchedBy(func(req ExecuteRequest) bool { return string(req.Inputs["item"].JSON) == fmt.Sprint(v) }),
		).Return(
			ExecuteResult{Outputs: contract.Values{"value": jsonValue(v)}},
			nil,
		).After(time.Duration(v) * time.Second).Once()
	}

	result := h.run(
		t,
		plan(outputGraph(
			map[string]contract.Node{"map": node},
			"nodes.map.outputs.result",
			`{"type":"array","items":{"type":"integer"}}`,
		)),
		nil,
	)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `[3,1,2]`)
}

func TestForeachEmptyDoesNotInvokeBody(t *testing.T) {
	h := newHarness(t)
	body := outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`)
	node := contract.Node{
		Type:    "foreach",
		Inputs:  map[string]contract.Port{"items": port(`{"type":"array"}`, literal([]any{}))},
		Outputs: map[string]contract.Port{"result": port(`{"type":"array"}`, nil)},
		Foreach: &contract.ForeachNode{Over: "items", Concurrency: 1, Body: body},
	}
	result := h.run(t, plan(outputGraph(map[string]contract.Node{"map": node}, "nodes.map.outputs.result", `{"type":"array"}`)), nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `[]`)
	if len(h.calls) != 0 {
		t.Fatal("empty foreach ran its body")
	}
}

func TestLoopUpdatesStateSimultaneouslyAndStopsBeforeNext(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(req ExecuteRequest) ExecuteResult { return ExecuteResult{Outputs: req.Inputs} }
	integer := `{"type":"integer"}`
	body := contract.Graph{
		Inputs: map[string]contract.Port{"a": port(integer, nil), "b": port(integer, nil)},
		Nodes: map[string]contract.Node{"copy": {
			Type:    "code",
			Code:    &contract.CodeNode{Command: []string{"copy"}},
			Inputs:  map[string]contract.Port{"a": port(integer, from("inputs.a")), "b": port(integer, from("inputs.b"))},
			Outputs: map[string]contract.Port{"a": port(integer, nil), "b": port(integer, nil)},
		}},
		Outputs: map[string]contract.Port{
			"a": port(integer, from("nodes.copy.outputs.a")),
			"b": port(integer, from("nodes.copy.outputs.b")),
		},
	}
	a, b := port(integer, nil), port(integer, nil)
	a.Initial, a.Next = literal(1), from("state.b")
	b.Initial, b.Next = literal(2), from("state.a")
	node := contract.Node{
		Type: "loop",
		Loop: &contract.LoopNode{
			MaxIterations: 2,
			State:         map[string]contract.Port{"a": a, "b": b},
			With:          map[string]contract.Binding{"a": *from("state.a"), "b": *from("state.b")},
			Body:          body,
			Until:         "iteration.index == 1",
		},
		Outputs: map[string]contract.Port{
			"a":           port(integer, nil),
			"b":           port(integer, nil),
			"iterations":  port(integer, nil),
			"termination": port(`{"type":"string"}`, nil),
		},
	}
	graph := contract.Graph{
		Nodes: map[string]contract.Node{"repeat": node},
		Outputs: map[string]contract.Port{
			"a":           port(integer, from("nodes.repeat.outputs.a")),
			"b":           port(integer, from("nodes.repeat.outputs.b")),
			"iterations":  port(integer, from("nodes.repeat.outputs.iterations")),
			"termination": port(`{"type":"string"}`, from("nodes.repeat.outputs.termination")),
		},
	}
	result := h.run(t, plan(graph), nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["a"], `2`)
	assertJSON(t, result.Outputs["b"], `1`)
	assertJSON(t, result.Outputs["iterations"], `2`)
	assertJSON(t, result.Outputs["termination"], `"condition"`)
}

func TestHumanRejectsInvalidAndDeduplicates(t *testing.T) {
	h := newHarness(t)
	node := contract.Node{
		Type:    "human",
		Human:   &contract.HumanNode{Prompt: contract.TextSource{Text: "Choose"}},
		Outputs: map[string]contract.Port{"value": port(`{"type":"string"}`, nil)},
	}
	id := stableID("h", rootID("review")+"/human")
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			HumanSignalName,
			HumanSignal{RequestID: id, ResponseID: "bad", Values: contract.Values{"value": jsonValue(123)}},
		)
	}, time.Second)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			HumanSignalName,
			HumanSignal{RequestID: id, ResponseID: "bad", Values: contract.Values{"value": jsonValue("changed")}},
		)
	}, 2*time.Second)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			HumanSignalName,
			HumanSignal{
				RequestID:  id,
				ResponseID: "good",
				Values:     contract.Values{"value": jsonValue("approved")},
			},
		)
	}, 3*time.Second)
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"review": node}, "nodes.review.outputs.value", `{"type":"string"}`)),
		nil,
	)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `"approved"`)
	accepted, rejected := 0, 0

	for _, req := range h.requests {
		if req.Status == "accepted" {
			accepted++
		}
	}

	for _, event := range h.events {
		if event.Status == "rejected" {
			rejected++
		}
	}

	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestHumanAcceptedBeforeDeadlineSurvivesDelayedDelivery(t *testing.T) {
	h := newHarness(t)
	node := contract.Node{
		Type:      "human",
		Execution: contract.Execution{Timeout: "5s"},
		Human:     &contract.HumanNode{},
		Outputs:   map[string]contract.Port{"value": port(`{"type":"string"}`, nil)},
	}
	h.answer = &HumanSignal{
		RequestID:  stableID("h", rootID("review")+"/human"),
		ResponseID: "saved",
		AcceptedAt: h.env.Now().Add(time.Second),
		Values:     contract.Values{"value": jsonValue("saved")},
	}
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"review": node}, "nodes.review.outputs.value", `{"type":"string"}`)),
		nil,
	)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `"saved"`)
}

func TestOnlyExplicitSafeFailureRetries(t *testing.T) {
	for _, retryable := range []bool{true, false} {
		t.Run(fmt.Sprint(retryable), func(t *testing.T) {
			h := newHarness(t)
			h.leaf = func(req ExecuteRequest) ExecuteResult {
				if req.Attempt == 1 {
					return ExecuteResult{Failure: &Failure{Code: "TRANSPORT", Message: "failed before sending", Retryable: retryable}}
				}
				return ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}}
			}
			node := llm()
			node.Execution.Retry = &contract.Retry{MaxAttempts: 2, Backoff: "1ms"}
			result := h.run(t, plan(outputGraph(map[string]contract.Node{"work": node}, "nodes.work.outputs.value", `{"type":"string"}`)), nil)
			if retryable {
				if result.Status != "succeeded" || len(h.calls) != 2 {
					t.Fatalf("%+v calls=%d", result, len(h.calls))
				}
			} else {
				if result.Status != "failed" || len(h.calls) != 1 {
					t.Fatalf("%+v calls=%d", result, len(h.calls))
				}
			}
		})
	}
}

func TestUnknownOutcomePausesAndAcceptsEvidence(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(_ ExecuteRequest) ExecuteResult {
		return ExecuteResult{Failure: &Failure{Code: "OUTCOME_UNKNOWN", Unknown: true, OperationID: "operation"}}
	}
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			ResolveSignalName,
			ResolutionSignal{
				InstanceID:  rootID("work"),
				OperationID: "operation",
				ResponseID:  "resolved",
				Decision:    "completed",
				Evidence:    "external record 42",
				Outputs:     contract.Values{"value": jsonValue("confirmed")},
			},
		)
	}, time.Second)
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`)),
		nil,
	)
	if result.Status != "succeeded" || len(h.calls) != 1 {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `"confirmed"`)
}

func TestResolutionAcceptedBeforeDeadlineSurvivesDelayedDelivery(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			h := newHarness(t)
			h.leaf = func(ExecuteRequest) ExecuteResult {
				return ExecuteResult{Failure: &Failure{Code: "OUTCOME_UNKNOWN", Unknown: true, OperationID: "operation"}}
			}
			node := llm()
			node.Execution.Timeout = "5s"
			acceptedAt := h.env.Now().Add(time.Second)
			if late {
				acceptedAt = h.env.Now().Add(6 * time.Second)
			}
			h.resolution = &ResolutionSignal{
				InstanceID:  rootID("work"),
				OperationID: "operation",
				ResponseID:  "saved",
				Decision:    "completed",
				Evidence:    "external record 42",
				AcceptedAt:  acceptedAt,
				Outputs:     contract.Values{"value": jsonValue("confirmed")},
			}
			result := h.run(t, plan(outputGraph(map[string]contract.Node{"work": node}, "nodes.work.outputs.value", `{"type":"string"}`)), nil)
			if len(h.calls) != 1 {
				t.Fatalf("replayed external operation: calls=%d", len(h.calls))
			}
			if late {
				if result.Status != "failed" || result.Failure.Code != "DEADLINE_EXCEEDED" {
					t.Fatalf("late response accepted: %+v", result)
				}
			} else {
				if result.Status != "succeeded" {
					t.Fatalf("timely response lost: %+v", result)
				}
				assertJSON(t, result.Outputs["result"], `"confirmed"`)
			}
		})
	}
}

func TestNotExecutedDoesNotRestartAgentWithUnsafePrefix(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(_ ExecuteRequest) ExecuteResult {
		return ExecuteResult{Failure: &Failure{Code: "OUTCOME_UNKNOWN", Unknown: true, OperationID: "operation"}}
	}
	node := llm()
	node.Type = "agent"
	node.LLM = nil
	node.Agent = &contract.AgentNode{MaxSteps: 3}
	node.Execution.Retry = &contract.Retry{MaxAttempts: 3}
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			ResolveSignalName,
			ResolutionSignal{
				InstanceID:  rootID("work"),
				OperationID: "operation",
				ResponseID:  "resolved",
				Decision:    "not_executed",
				Evidence:    "verified operation never ran",
			},
		)
	}, time.Second)
	result := h.run(t, plan(outputGraph(map[string]contract.Node{"work": node}, "nodes.work.outputs.value", `{"type":"string"}`)), nil)
	if result.Status != "failed" || len(h.calls) != 1 {
		t.Fatalf("unsafe attempt restarted: %+v calls=%d", result, len(h.calls))
	}
}

func TestNestedPipelineScopesAndPermissions(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(req ExecuteRequest) ExecuteResult {
		if len(req.Scopes) != 2 || req.Scopes[1].Limits.MaxModelCalls != 2 || req.Permissions == nil || len(req.Permissions.Models) != 1 || req.Permissions.Models[0] != "allowed" {
			return ExecuteResult{Failure: failure("TEST", fmt.Sprintf("bad scope: %+v", req))}
		}
		return ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}}
	}
	child := &contract.Pipeline{Spec: contract.Spec{
		Limits: contract.Limits{MaxModelCalls: 2},
		Graph:  outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`),
	}}
	node := contract.Node{
		Type: "pipeline",
		Pipeline: &contract.PipelineNode{
			File:        "child.yaml",
			Permissions: contract.Permissions{Models: []string{"allowed"}},
		},
		Outputs: map[string]contract.Port{"result": port(`{"type":"string"}`, nil)},
	}
	p := plan(outputGraph(map[string]contract.Node{"child": node}, "nodes.child.outputs.result", `{"type":"string"}`))
	p.Pipelines["child.yaml"] = child
	result := h.run(t, p, nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
}

func TestNodeBudgetIncludesSkippedAndCompositeInstances(t *testing.T) {
	h := newHarness(t)
	a, b := llm(), llm()
	a.When, b.When = "false", "false"
	p := plan(outputGraph(map[string]contract.Node{"a": a, "b": b}, "nodes.a.outputs.value", `{"type":"string"}`))
	p.Profile.Spec.Limits.MaxNodeInstances = 1
	result := h.run(t, p, nil)
	if result.Status != "failed" || result.Failure.Code != "LIMIT_EXCEEDED" || len(h.calls) != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestCancelClosesHumanRequest(t *testing.T) {
	h := newHarness(t)
	node := contract.Node{
		Type:    "human",
		Human:   &contract.HumanNode{},
		Outputs: map[string]contract.Port{"value": port(`{"type":"string"}`, nil)},
	}
	h.env.RegisterDelayedCallback(
		func() { h.env.SignalWorkflow(CancelSignalName, CancelSignal{Reason: "stop"}) },
		time.Second,
	)
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"review": node}, "nodes.review.outputs.value", `{"type":"string"}`)),
		nil,
	)
	if result.Status != "cancelled" {
		t.Fatalf("%+v", result)
	}
	if len(h.requests) < 2 || h.requests[len(h.requests)-1].Status != "cancelled" {
		t.Fatalf("request remained open: %+v", h.requests)
	}
}
