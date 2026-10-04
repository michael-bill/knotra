package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"go.temporal.io/sdk/workflow"
)

func TestInstanceObservationMetadataAndLegacyVersion(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			h := newHarness(t)
			if legacy {
				h.env.OnGetVersion("instance-observability", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			leaf := llm()
			leaf.Inputs = map[string]contract.Port{"item": port(`{"type":"integer"}`, from("inputs.item"))}
			body := outputGraph(map[string]contract.Node{"work": leaf}, "nodes.work.outputs.value", `{"type":"string"}`)
			body.Inputs = map[string]contract.Port{"item": port(`{"type":"integer"}`, nil)}
			node := contract.Node{Type: "foreach", Inputs: map[string]contract.Port{"items": port(`{"type":"array"}`, literal([]int{4, 8}))}, Outputs: map[string]contract.Port{"result": port(`{"type":"array"}`, nil)}, Foreach: &contract.ForeachNode{Over: "items", Concurrency: 2, With: map[string]contract.Binding{"item": *from("iteration.item")}, Body: body}}
			result := h.run(t, plan(outputGraph(map[string]contract.Node{"map": node}, "nodes.map.outputs.result", `{"type":"array"}`)), nil)
			if result.Status != "succeeded" {
				t.Fatal(result)
			}
			seen := map[int]bool{}
			for _, event := range h.events {
				if event.NodeID != "work" || event.Status != "succeeded" {
					continue
				}
				if legacy {
					if event.ParentInstanceID != "" || event.GraphPath != "" || event.Inputs != nil {
						t.Fatal("legacy workflow gained observation payload", event)
					}
					continue
				}
				if event.ParentInstanceID != rootID("map") || event.IterationIndex == nil || event.GraphPath != "/nodes/map/body/nodes/work" || event.NodeType != "llm" || event.StartedAt == nil || event.FinishedAt == nil || event.FinishedAt.Before(*event.StartedAt) {
					t.Fatalf("missing child metadata %+v", event)
				}
				seen[*event.IterationIndex] = true
				if event.Inputs["item"].JSON == nil {
					t.Fatal("bound node input missing")
				}
			}
			if !legacy && (!seen[0] || !seen[1]) {
				t.Fatal("iteration instances are ambiguous", seen)
			}
		})
	}
}

func TestObservationInputsDoNotCopyUnboundedValuesOrArtifactPaths(t *testing.T) {
	huge, _ := json.Marshal(strings.Repeat("x", 40<<10))
	values := contract.Values{"large": {JSON: huge}, "small": jsonValue(42), "file": {Artifacts: []contract.Artifact{{ID: "artifact", Path: "/private/attempt"}}}}
	preview, truncated := observationInputs(values)
	if !truncated || len(preview) != 2 || preview["file"].Artifacts[0].Path != "" || values["file"].Artifacts[0].Path == "" {
		t.Fatal(preview, truncated)
	}
}

func TestObservationMetadataRecoversLegacyCheckpointAncestors(t *testing.T) {
	h := newHarness(t)
	body := outputGraph(map[string]contract.Node{"work": llm()}, "nodes.work.outputs.value", `{"type":"string"}`)
	node := contract.Node{
		Type:    "foreach",
		Inputs:  map[string]contract.Port{"items": port(`{"type":"array"}`, literal([]int{1}))},
		Outputs: map[string]contract.Port{"result": port(`{"type":"array"}`, nil)},
		Foreach: &contract.ForeachNode{Over: "items", Concurrency: 1, Body: body},
	}
	parentID := rootID("map")
	saved := &Checkpoint{
		State: Snapshot{RunID: "test", Status: "running", Nodes: map[string]*NodeSnapshot{
			parentID: {ID: parentID, NodeID: "map", Pipeline: "pipeline.yaml", Status: "pending"},
		}, Requests: map[string]*Request{}},
		Sequence: 2, Deadline: time.Now().Add(time.Hour), NodeCounts: map[string]int{"test": 1}, Projected: map[string]bool{parentID: true},
	}
	h.env.ExecuteWorkflow(Workflow, RunInput{
		RunID: "test", Plan: plan(outputGraph(map[string]contract.Node{"map": node}, "nodes.map.outputs.result", `{"type":"array"}`)), Checkpoint: saved,
	})
	if err := h.env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := h.env.GetWorkflowResult(&result); err != nil || result.Status != "succeeded" {
		t.Fatal(result, err)
	}
	seen := false
	for _, event := range h.events {
		if event.Sequence <= saved.Sequence {
			t.Fatal("projection sequence restarted")
		}
		if event.NodeID == "map" && event.Status == "pending" {
			t.Fatal("checkpointed parent materialized twice")
		}
		if event.NodeID == "work" {
			seen = true
			if event.GraphPath != "/nodes/map/body/nodes/work" || event.ParentInstanceID != parentID {
				t.Fatal("lost legacy ancestor", event)
			}
		}
	}
	if !seen {
		t.Fatal("missing child execution")
	}
}
