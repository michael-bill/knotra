package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestWorkflowLoadsAdmittedPlanFromIdentity(t *testing.T) {
	h := newHarness(t)
	p := plan(outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`))
	h.env.RegisterActivityWithOptions(func(_ context.Context, request PlanRequest) (contract.Plan, error) {
		if request.RunID != "test" {
			return contract.Plan{}, errors.New("wrong run identity")
		}
		return p, nil
	}, activity.RegisterOptions{Name: PlanActivity})
	h.env.ExecuteWorkflow(Workflow, RunInput{RunID: "test", AcceptedAt: h.env.Now()})
	if err := h.env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := h.env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" || len(h.calls) != 1 || h.calls[0].Plan.Version != "" {
		t.Fatalf("plan load failed or leaf repeated plan: %+v", result)
	}
}

func TestIncompatiblePlanPublishesFailureWithoutExternalWork(t *testing.T) {
	h := newHarness(t)
	p := plan(outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`))
	p.CompilerVersion = "future-compiler"
	result := h.run(t, p, nil)
	if result.Status != "failed" || result.Failure.Code != "PLAN_VERSION_UNSUPPORTED" || len(h.calls) != 0 || len(h.events) != 1 || h.events[0].Status != "failed" {
		t.Fatalf("incompatible plan lost durable terminal state: %+v events=%+v", result, h.events)
	}
}

func runWithCheckpoints(t *testing.T, p contract.Plan, leaf func(ExecuteRequest) ExecuteResult) (RunResult, []ExecuteRequest, []Projection, int) {
	t.Helper()
	input := RunInput{RunID: "test", Plan: p}
	var calls []ExecuteRequest
	var events []Projection
	var originalDeadline time.Time

	for generation := 0; generation < 10; generation++ {
		h := newHarness(t)
		h.leaf = leaf
		h.onProject = func(event Projection) {
			if event.Kind == "node" && event.Status == "succeeded" {
				h.env.SetCurrentHistoryLength(checkpointHistoryEvents)
			}
		}
		h.env.ExecuteWorkflow(Workflow, input)
		calls = append(calls, h.calls...)
		events = append(events, h.events...)
		err := h.env.GetWorkflowError()
		var continued *workflow.ContinueAsNewError
		if errors.As(err, &continued) {
			if err := converter.GetDefaultDataConverter().FromPayloads(continued.Input, &input); err != nil {
				t.Fatal(err)
			}
			if input.Checkpoint == nil {
				t.Fatal("missing checkpoint")
			}
			if originalDeadline.IsZero() {
				originalDeadline = input.Checkpoint.Deadline
			} else if !originalDeadline.Equal(input.Checkpoint.Deadline) {
				t.Fatal("continuation extended deadline")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var result RunResult
		if err := h.env.GetWorkflowResult(&result); err != nil {
			t.Fatal(err)
		}
		return result, calls, events, generation
	}

	t.Fatal("checkpoint loop did not converge")
	return RunResult{}, nil, nil, 0
}

func TestContinueAsNewDoesNotRepeatCompletedLeavesOrBudgets(t *testing.T) {
	a, b, c := llm(), llm(), llm()
	b.Dependencies, b.Needs = []string{"a"}, []string{"a"}
	c.Dependencies, c.Needs = []string{"b"}, []string{"b"}
	p := plan(outputGraph(map[string]contract.Node{"a": a, "b": b, "c": c}, "nodes.c.outputs.value", `{"type":"string"}`))
	p.Profile.Spec.Limits.MaxNodeInstances = 3
	result, calls, events, continued := runWithCheckpoints(t, p, nil)
	if result.Status != "succeeded" || len(calls) != 3 || continued < 2 {
		t.Fatalf("result=%+v calls=%d continuations=%d", result, len(calls), continued)
	}
	seenSequences := map[int64]bool{}
	pending, succeeded := map[string]int{}, map[string]int{}

	for _, event := range events {
		if seenSequences[event.Sequence] {
			t.Fatalf("duplicate projection sequence %d", event.Sequence)
		}
		seenSequences[event.Sequence] = true
		if event.Kind == "node" {
			if event.Status == "pending" {
				pending[event.InstanceID]++
			}
			if event.Status == "succeeded" {
				succeeded[event.InstanceID]++
			}
		}
	}

	for _, call := range calls {
		if pending[call.InstanceID] != 1 || succeeded[call.InstanceID] != 1 {
			t.Fatalf("duplicated lifecycle for %s", call.InstanceID)
		}
	}
}

func TestContinueAsNewRestoresLoopCursor(t *testing.T) {
	integer := `{"type":"integer"}`
	bodyLeaf := llm()
	bodyLeaf.Inputs = map[string]contract.Port{"value": port(integer, from("inputs.value"))}
	bodyLeaf.Outputs = map[string]contract.Port{"value": port(integer, nil)}
	body := outputGraph(map[string]contract.Node{"step": bodyLeaf}, "nodes.step.outputs.value", integer)
	body.Inputs = map[string]contract.Port{"value": port(integer, nil)}
	carry := port(integer, nil)
	carry.Initial, carry.Next = literal(0), expression("body.outputs.result + 1")
	node := contract.Node{
		Type: "loop",
		Loop: &contract.LoopNode{
			MaxIterations: 3,
			State:         map[string]contract.Port{"value": carry},
			With:          map[string]contract.Binding{"value": *from("state.value")},
			Body:          body,
			Until:         "body.outputs.result == 2",
		},
		Outputs: map[string]contract.Port{
			"result":      port(integer, nil),
			"iterations":  port(integer, nil),
			"termination": port(`{"type":"string"}`, nil),
		},
	}
	p := plan(outputGraph(map[string]contract.Node{"repeat": node}, "nodes.repeat.outputs.result", integer))
	p.Profile.Spec.Limits.MaxNodeInstances = 4
	result, calls, _, continued := runWithCheckpoints(t, p, func(req ExecuteRequest) ExecuteResult { return ExecuteResult{Outputs: req.Inputs} })
	if result.Status != "succeeded" || len(calls) != 3 || continued < 2 {
		t.Fatalf("result=%+v calls=%d continuations=%d", result, len(calls), continued)
	}
	assertJSON(t, result.Outputs["result"], `2`)
}

func TestContinueAsNewRestoresForeachCompletedItems(t *testing.T) {
	integer := `{"type":"integer"}`
	bodyLeaf := llm()
	bodyLeaf.Inputs = map[string]contract.Port{"value": port(integer, from("inputs.value"))}
	bodyLeaf.Outputs = map[string]contract.Port{"value": port(integer, nil)}
	body := outputGraph(map[string]contract.Node{"step": bodyLeaf}, "nodes.step.outputs.value", integer)
	body.Inputs = map[string]contract.Port{"value": port(integer, nil)}
	node := contract.Node{
		Type:   "foreach",
		Inputs: map[string]contract.Port{"values": port(`{"type":"array"}`, literal([]int{3, 1, 2}))},
		Foreach: &contract.ForeachNode{
			Over:        "values",
			Concurrency: 1,
			With:        map[string]contract.Binding{"value": *from("iteration.item")},
			Body:        body,
		},
		Outputs: map[string]contract.Port{"result": port(`{"type":"array"}`, nil)},
	}
	p := plan(outputGraph(map[string]contract.Node{"map": node}, "nodes.map.outputs.result", `{"type":"array"}`))
	p.Profile.Spec.Limits.MaxNodeInstances = 4
	result, calls, _, continued := runWithCheckpoints(t, p, func(req ExecuteRequest) ExecuteResult { return ExecuteResult{Outputs: req.Inputs} })
	if result.Status != "succeeded" || len(calls) != 3 || continued < 2 {
		t.Fatalf("result=%+v calls=%d continuations=%d", result, len(calls), continued)
	}
	assertJSON(t, result.Outputs["result"], `[3,1,2]`)
}

func TestExpiredAdmissionDoesNotStartWork(t *testing.T) {
	h := newHarness(t)
	p := plan(outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`))
	p.Profile.Spec.Limits.Timeout = "1s"
	h.env.ExecuteWorkflow(Workflow, RunInput{RunID: "test", AcceptedAt: h.env.Now().Add(-time.Hour), Plan: p})
	if err := h.env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := h.env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Failure.Code != "DEADLINE_EXCEEDED" || len(h.calls) != 0 {
		t.Fatalf("expired admission executed work: %+v", result)
	}
}
