package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestProjectionSurvivesDatabaseOutageLongerThanFiveMinutes(t *testing.T) {
	h := newHarness(t)
	start := h.env.Now()
	var attempts atomic.Int32
	h.env.OnActivity(ProjectActivity, mock.Anything, mock.Anything).Return(func(_ context.Context, p Projection) error {
		if p.Kind == "node" && p.NodeID == "first" && p.Status == "succeeded" && attempts.Add(1) == 1 {
			// The SDK test environment caps unlimited policies to 10 attempts.
			// A server-directed retry delay exercises a >5m outage in two attempts.
			return temporal.NewApplicationErrorWithOptions(
				"database is temporarily unavailable",
				"DB_OUTAGE",
				temporal.ApplicationErrorOptions{NextRetryDelay: 6 * time.Minute},
			)
		}
		return nil
	})
	first, next := llm(), llm()
	first.Execution.Timeout = "20m"
	next.Dependencies, next.Needs = []string{"first"}, []string{"first"}
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"first": first, "next": next}, "nodes.next.outputs.value", `{"type":"string"}`)),
		nil,
	)
	if result.Status != "succeeded" || attempts.Load() != 2 || h.env.Now().Sub(start) <= 5*time.Minute || len(h.calls) != 2 {
		t.Fatalf(
			"durable publication lost: result=%+v attempts=%d elapsed=%s calls=%d",
			result,
			attempts.Load(),
			h.env.Now().Sub(start),
			len(h.calls),
		)
	}
}

func TestNodeTimersFollowDurableAdmission(t *testing.T) {
	h := newHarness(t)
	var ready atomic.Int32
	h.onProject = func(p Projection) {
		if p.Kind == "node" && p.Status == "ready" {
			ready.Add(1)
		}
	}
	timers := 0
	h.env.SetOnTimerScheduledListener(func(_ string, duration time.Duration) {
		if duration == time.Hour {
			return
		} // root deadline
		timers++
		if int(ready.Load()) < timers {
			t.Errorf("timer %d preceded durable node admission (%d ready)", timers, ready.Load())
		}
	})
	nodes := map[string]contract.Node{}

	for i := range 100 {
		nodes[fmt.Sprintf("node_%03d", i)] = llm()
	}

	result := h.run(t, plan(outputGraph(nodes, "nodes.node_099.outputs.value", `{"type":"string"}`)), nil)
	if result.Status != "succeeded" || timers != 100 {
		t.Fatalf("result=%+v nodeTimers=%d", result, timers)
	}
}

func TestLoopLimitModes(t *testing.T) {
	for _, mode := range []string{"fail", "return_last"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			body := outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`)
			node := contract.Node{
				Type: "loop",
				Loop: &contract.LoopNode{MaxIterations: 2, OnLimit: mode, Body: body, Until: "false"},
				Outputs: map[string]contract.Port{
					"result":      port(`{"type":"string"}`, nil),
					"iterations":  port(`{"type":"integer"}`, nil),
					"termination": port(`{"type":"string"}`, nil),
				},
			}
			result := h.run(
				t,
				plan(outputGraph(map[string]contract.Node{"repeat": node}, "nodes.repeat.outputs.termination", `{"type":"string"}`)),
				nil,
			)
			if len(h.calls) != 2 {
				t.Fatalf("wrong body count: %d", len(h.calls))
			}
			if mode == "fail" {
				if result.Status != "failed" || result.Failure.Code != "LIMIT_EXCEEDED" || result.Outputs != nil {
					t.Fatalf("%+v", result)
				}
			} else {
				if result.Status != "succeeded" {
					t.Fatalf("%+v", result)
				}
				assertJSON(t, result.Outputs["result"], `"limit"`)
			}
		})
	}
}

func TestMissingRequiredExportFailsButOptionalIsOmitted(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(map[bool]string{true: "required", false: "optional"}[required], func(t *testing.T) {
			h := newHarness(t)
			node := llm()
			node.When = "false"
			graph := outputGraph(map[string]contract.Node{"work": node}, "nodes.work.outputs.value", `{"type":"string"}`)
			p := graph.Outputs["result"]
			p.Required = &required
			graph.Outputs["result"] = p
			result := h.run(t, plan(graph), nil)
			if required {
				if result.Status != "failed" || result.Failure.Code != "OUTPUT_UNAVAILABLE" {
					t.Fatalf("%+v", result)
				}
			} else {
				if result.Status != "succeeded" || len(result.Outputs) != 0 {
					t.Fatalf("%+v", result)
				}
			}
		})
	}
}

func TestDirectToolReceivesEvaluatedArguments(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(request ExecuteRequest) ExecuteResult {
		assertJSON(t, contract.Value{JSON: request.ToolArguments}, `{"query":"hello"}`)
		return ExecuteResult{Outputs: contract.Values{"result": jsonValue(7)}}
	}
	node := contract.Node{
		Type:    "tool",
		Inputs:  map[string]contract.Port{"query": port(`{"type":"string"}`, literal("hello"))},
		Tool:    &contract.ToolNode{Server: "search", Name: "search", Arguments: *expression(`{"query": args.query}`)},
		Outputs: map[string]contract.Port{"result": port(`{"type":"integer"}`, nil)},
	}
	result := h.run(
		t,
		plan(outputGraph(map[string]contract.Node{"search": node}, "nodes.search.outputs.result", `{"type":"integer"}`)),
		nil,
	)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `7`)
}

func TestInvalidOutputStopsDependentBeforeEffects(t *testing.T) {
	h := newHarness(t)
	h.leaf = func(request ExecuteRequest) ExecuteResult {
		return ExecuteResult{Outputs: contract.Values{"value": jsonValue(123)}}
	}
	a, b := llm(), llm()
	b.Dependencies, b.Needs = []string{"a"}, []string{"a"}
	result := h.run(t, plan(outputGraph(map[string]contract.Node{"a": a, "b": b}, "nodes.b.outputs.value", `{"type":"string"}`)), nil)
	if result.Status != "failed" || result.Failure.Code != "OUTPUT_INVALID" || len(h.calls) != 1 {
		t.Fatalf("%+v calls=%d", result, len(h.calls))
	}
}

func TestConcurrencyLimitQueuesLeafAttempts(t *testing.T) {
	h := newHarness(t)
	h.env.OnActivity(ExecuteActivity, mock.Anything, mock.Anything).Return(
		ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}},
		nil,
	).After(time.Second).Times(3)
	p := plan(outputGraph(map[string]contract.Node{"a": llm(), "b": llm(), "c": llm()}, "nodes.c.outputs.value", `{"type":"string"}`))
	p.Profile.Spec.Limits.MaxConcurrentNodes = 1
	result := h.run(t, p, nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	var starts []time.Time

	for _, event := range h.events {
		if event.Kind == "node" && event.Status == "running" && event.Attempt == 1 {
			starts = append(starts, event.Time)
		}
	}

	if len(starts) != 3 {
		t.Fatalf("starts=%d", len(starts))
	}

	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < time.Second {
			t.Fatal("leaf concurrency budget was exceeded")
		}
	}
}

func TestUnknownOutcomeBlocksQueuedLeaf(t *testing.T) {
	h := newHarness(t)
	var unknownInstance atomic.Value
	unknownInstance.Store("")
	h.leaf = func(request ExecuteRequest) ExecuteResult {
		// Independent ready projections can complete in either order. Whichever
		// leaf acquires the first execution slot must block the remaining leaf.
		if unknownInstance.CompareAndSwap("", request.InstanceID) {
			return ExecuteResult{Failure: &Failure{Code: "UNKNOWN", Unknown: true, OperationID: "op"}}
		}
		return ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}}
	}
	p := plan(outputGraph(map[string]contract.Node{"a": llm(), "b": llm()}, "nodes.b.outputs.value", `{"type":"string"}`))
	p.Profile.Spec.Limits.MaxConcurrentNodes = 1
	resumeAt := h.env.Now().Add(2 * time.Second)
	h.env.RegisterDelayedCallback(func() {
		instanceID := unknownInstance.Load().(string)
		if instanceID == "" {
			t.Error("no leaf produced an unknown outcome before resolution")
			return
		}
		h.env.SignalWorkflow(
			ResolveSignalName,
			ResolutionSignal{
				InstanceID:  instanceID,
				OperationID: "op",
				Decision:    "completed",
				ResponseID:  "evidence",
				Evidence:    "confirmed",
				Outputs:     contract.Values{"value": jsonValue("ok")},
			},
		)
	}, 2*time.Second)
	result := h.run(t, p, nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	if len(h.calls) != 2 {
		t.Fatalf("external calls=%d, want 2", len(h.calls))
	}

	unknownID := unknownInstance.Load().(string)
	queuedStarts := 0
	for _, event := range h.events {
		if event.Kind == "node" && event.InstanceID != unknownID && event.Status == "running" && event.Attempt == 1 {
			queuedStarts++
			if event.Time.Before(resumeAt) {
				t.Fatal("new external work started while resolution was pending")
			}
		}
	}
	if queuedStarts != len(h.calls)-1 {
		t.Fatalf("queued starts=%d, want %d", queuedStarts, len(h.calls)-1)
	}
}

func TestHumanWaitDoesNotHoldExecutionSlot(t *testing.T) {
	h := newHarness(t)
	human := contract.Node{
		Type:    "human",
		Human:   &contract.HumanNode{},
		Outputs: map[string]contract.Port{"value": port(`{"type":"string"}`, nil)},
	}
	p := plan(outputGraph(
		map[string]contract.Node{"a_human": human, "b_work": llm()},
		"nodes.a_human.outputs.value",
		`{"type":"string"}`,
	))
	p.Profile.Spec.Limits.MaxConcurrentNodes = 1
	answerAt := h.env.Now().Add(time.Second)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(
			HumanSignalName,
			HumanSignal{
				RequestID:  stableID("h", rootID("a_human")+"/human"),
				ResponseID: "answer",
				Values:     contract.Values{"value": jsonValue("ok")},
			},
		)
	}, time.Second)
	result := h.run(t, p, nil)
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	ran := false

	for _, event := range h.events {
		if event.NodeID == "b_work" && event.Status == "succeeded" && event.Time.Before(answerAt) {
			ran = true
		}
	}

	if !ran {
		t.Fatal("human held the leaf execution slot")
	}
}

func TestPermissionIntersectionCannotEscalate(t *testing.T) {
	parent := &contract.Permissions{
		Models:    []string{"allowed"},
		MCP:       map[string][]string{"server": {"read"}},
		Sandboxes: []string{"local"},
		Secrets:   []string{"safe"},
	}
	child := &contract.Permissions{
		Models:    []string{"allowed", "forbidden"},
		MCP:       map[string][]string{"server": {"read", "write"}, "extra": {"all"}},
		Sandboxes: []string{"local", "host"},
		Secrets:   []string{"safe", "root"},
	}
	actual := intersectPermissions(parent, child)
	if len(actual.Models) != 1 || len(actual.MCP["server"]) != 1 || len(actual.MCP["extra"]) != 0 || len(actual.Sandboxes) != 1 || len(actual.Secrets) != 1 {
		t.Fatalf("escalated permissions: %+v", actual)
	}
}
