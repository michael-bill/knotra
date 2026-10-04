package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/engine"
	"github.com/michael-bill/knotra/internal/protocol"
)

func TestObservationContextBoundsPortsAndHidesArtifactPaths(t *testing.T) {
	large, _ := json.Marshal(strings.Repeat("x", maxContextBytes))
	values := contract.Values{
		"small": {JSON: json.RawMessage(`{"value":42}`)},
		"large": {JSON: large},
		"file":  {Artifacts: []contract.Artifact{{ID: "artifact", Path: "/sandbox/private/file"}}},
	}
	preview, truncated := observationContext(values)
	data, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(data) > maxContextBytes || strings.Contains(string(data), "/sandbox") || strings.Contains(string(data), "large") || !strings.Contains(string(data), "42") {
		t.Fatalf("unexpected context preview: %s, truncated %v", data, truncated)
	}
	if values["file"].Artifacts[0].Path == "" {
		t.Fatal("preview modified executable inputs")
	}
}

func TestObservationContextOmitsUnrepresentablePortsWithoutChangingExecution(t *testing.T) {
	values := contract.Values{
		"safe":     {JSON: json.RawMessage(`{"number":9007199254740991,"fraction":1.25}`)},
		"unsafe":   {JSON: json.RawMessage(`{"nested":[-9007199254740993]}`)},
		"exponent": {JSON: json.RawMessage(`1e20`)},
	}
	preview, truncated := observationContext(values)
	data, err := json.Marshal(preview)
	if err != nil || !truncated || strings.Contains(string(data), "unsafe") || strings.Contains(string(data), "exponent") || !strings.Contains(string(data), "9007199254740991") {
		t.Fatalf("unsafe numeric preview: %s, truncated %v, error %v", data, truncated, err)
	}
	if string(values["unsafe"].JSON) != `{"nested":[-9007199254740993]}` {
		t.Fatal("executable value changed")
	}
}

func TestObservationsPreserveProjectionSequenceAndPagedHistory(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	projection := engine.Projection{RunID: runID, Sequence: 1, Kind: "node", InstanceID: "node", NodeID: "work", Pipeline: "pipeline.yaml", Status: "running", Time: time.Now()}
	if err := s.Project(ctx, projection); err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		instance := "node"
		if index == 1 {
			instance = "other"
		}
		event := protocol.Event{RunID: runID, InstanceID: instance, AttemptID: "node.a1", Type: "model.delta", Data: map[string]any{"content": fmt.Sprint(index)}}
		if err := s.Observe(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Project(ctx, projection); err != nil {
		t.Fatal(err)
	}
	projection.Sequence, projection.Status = 2, "succeeded"
	projection.Inputs = contract.Values{"input": {JSON: json.RawMessage(`"hello"`)}}
	projection.Outputs = contract.Values{"value": {JSON: json.RawMessage(`"done"`)}}
	if err := s.Project(ctx, projection); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(ctx, runID, "", "node")
	if err != nil || len(page) != 100 {
		t.Fatalf("first page %d %v", len(page), err)
	}
	next, err := s.History(ctx, runID, page[99].ID, "node")
	if err != nil || len(next) != 6 || next[len(next)-1].Message != "succeeded" {
		t.Fatalf("next page %+v %v", next, err)
	}
	for _, event := range append(page, next...) {
		if event.InstanceID != "node" {
			t.Fatal("filter leaked another instance")
		}
	}
	run, err := s.Run(ctx, runID)
	if err != nil || len(run.Instances) != 1 || run.Instances[0].Status != "succeeded" || run.Instances[0].Inputs == nil || run.Instances[0].Outputs == nil {
		t.Fatalf("projection overwritten by observations: %+v %v", run.Instances, err)
	}
	if err := s.Observe(ctx, protocol.Event{RunID: runID, InstanceID: "node", Type: "model.completed", Data: map[string]any{"step": 2, "content": strings.Repeat("x", maxObservationBytes)}}); err != nil {
		t.Fatal(err)
	}
	last, err := s.Events(ctx, runID, next[len(next)-1].ID)
	if err != nil || len(last) != 1 {
		t.Fatal(last, err)
	}
	b, _ := json.Marshal(last[0].Data)
	if !strings.Contains(string(b), `"truncated":true`) || !strings.Contains(string(b), `"step":2`) || len(b) > maxObservationBytes {
		t.Fatal(string(b))
	}
}

func TestObservationWaitsForRunLockBeforeAllocatingCursor(t *testing.T) {
	s := testStore(t)
	runID, _ := setupRun(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err = lockRun(ctx, tx, runID); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err = s.Observe(blocked, protocol.Event{RunID: runID, InstanceID: "node", Type: "model.delta"})
	if err == nil {
		t.Fatal("observation bypassed the projection lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, runID, "")
	if err != nil || len(events) != 0 {
		t.Fatal("cancelled observation published a cursor", events, err)
	}
}
