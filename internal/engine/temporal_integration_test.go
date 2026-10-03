package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/contract"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"
)

// Replay uses retained histories and the same payload store without running any
// activity or creating a workflow. It is useful when changing orchestration code.
func TestTemporalReplayRetainedHistory(t *testing.T) {
	id, firstRun := os.Getenv("KNOTRA_TEST_REPLAY_WORKFLOW"), os.Getenv("KNOTRA_TEST_REPLAY_RUN_ID")
	if id == "" || firstRun == "" {
		t.Skip("set retained stress workflow and first Temporal run IDs for replay")
	}
	if filepath.Base(id) != id {
		t.Fatal("invalid workflow ID")
	}
	root := filepath.Join("../..", ".knotra", "temporal-history", id)
	blobs, err := NewFileBlobStore(root)
	if err != nil {
		t.Fatal(err)
	}
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), &PayloadCodec{Store: blobs})
	address := os.Getenv("KNOTRA_TEST_TEMPORAL")
	if address == "" {
		address = "127.0.0.1:7233"
	}
	namespace := os.Getenv("KNOTRA_TEST_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: namespace, DataConverter: dc})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for runID := firstRun; runID != ""; {
		history, next := &historypb.History{}, ""
		iter := c.GetWorkflowHistory(ctx, id, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for iter.HasNext() {
			event, err := iter.Next()
			if err != nil {
				t.Fatal(err)
			}
			history.Events = append(history.Events, event)
			if continued := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); continued != nil {
				next = continued.NewExecutionRunId
			}
		}
		replayer, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: dc})
		if err != nil {
			t.Fatal(err)
		}
		replayer.RegisterWorkflowWithOptions(Workflow, workflow.RegisterOptions{Name: WorkflowName})
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatalf("replay %s: %v", runID, err)
		}
		t.Logf("replayed %s with %d events and no external activity calls", runID, len(history.Events))
		runID = next
	}
}

// This exercises a real Temporal server and durable payload files. Leaf I/O is
// deliberately simulated: the target is orchestration, history and payload size,
// not provider throughput or model quality. Other acceptance tests cover those.
func TestTemporalLargePlanAndHistoryContinuation(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_TEMPORAL_STRESS") != "1" {
		t.Skip("set KNOTRA_TEST_TEMPORAL_STRESS=1 for real Temporal history/payload acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	id := "knotra-history-" + uuid.NewString()
	root, err := filepath.Abs(filepath.Join("../..", ".knotra", "temporal-history", id))
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := NewFileBlobStore(root)
	if err != nil {
		t.Fatal(err)
	}
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), &PayloadCodec{Store: blobs})
	address := os.Getenv("KNOTRA_TEST_TEMPORAL")
	if address == "" {
		address = "127.0.0.1:7233"
	}
	namespace := os.Getenv("KNOTRA_TEST_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: namespace, DataConverter: dc})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	nodes := map[string]contract.Node{}
	for i := range 1000 {
		nodes[fmt.Sprintf("switch_%04d", i)] = contract.Node{Type: "switch", Switch: &contract.SwitchNode{Default: "done"}, Outputs: map[string]contract.Port{"route": port(`{"type":"string"}`, nil)}}
	}
	for i := range 4 {
		node := llm()
		node.LLM.Prompt.Text = strings.Repeat(fmt.Sprintf("Prompt %d data. ", i), 60000)
		nodes[fmt.Sprintf("model_%d", i)] = node
	}
	p := plan(outputGraph(nodes, "nodes.switch_0999.outputs.route", `{"type":"string"}`))
	p.Profile.Spec.Limits.MaxNodeInstances = len(nodes)
	for i := range 4 {
		p.Package.Files = append(p.Package.Files, contract.File{Path: fmt.Sprintf("prompts/%d.txt", i), Content: []byte(nodes[fmt.Sprintf("model_%d", i)].LLM.Prompt.Text)})
	}
	var mu sync.Mutex
	leafCalls := map[string]int{}
	successes := map[string]int{}
	planLoads := 0
	w := worker.New(c, id, worker.Options{})
	w.RegisterWorkflowWithOptions(Workflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(func(context.Context, PlanRequest) (contract.Plan, error) {
		mu.Lock()
		planLoads++
		mu.Unlock()
		return p, nil
	}, activity.RegisterOptions{Name: PlanActivity})
	w.RegisterActivityWithOptions(func(_ context.Context, req ExecuteRequest) (ExecuteResult, error) {
		if req.Plan.Version != "" || req.Node.LLM == nil || len(req.Node.LLM.Prompt.Text) < 512<<10 {
			return ExecuteResult{Failure: failure("TEST_INVALID", "leaf request lost prompt or repeated full plan")}, nil
		}
		mu.Lock()
		leafCalls[req.InstanceID]++
		mu.Unlock()
		return ExecuteResult{Outputs: contract.Values{"value": jsonValue("ok")}}, nil
	}, activity.RegisterOptions{Name: ExecuteActivity})
	w.RegisterActivityWithOptions(func(_ context.Context, event Projection) error {
		if event.Kind == "node" && event.Status == "succeeded" {
			mu.Lock()
			successes[event.InstanceID]++
			mu.Unlock()
		}
		return nil
	}, activity.RegisterOptions{Name: ProjectActivity})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: id, StaticSummary: "Knotra · 1004 nodes and large prompts"}, WorkflowName, RunInput{RunID: id, AcceptedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	firstRunID := run.GetRunID()
	defer func() {
		if t.Failed() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			_ = c.TerminateWorkflow(cleanup, id, "", "stress test ended")
		}
	}()
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	assertJSON(t, result.Outputs["result"], `"done"`)
	mu.Lock()
	if len(successes) != len(nodes) || len(leafCalls) != 4 || planLoads < 2 {
		nodeCount, leafCount, loads := len(successes), len(leafCalls), planLoads
		mu.Unlock()
		t.Fatalf("nodes=%d leaves=%d planLoads=%d", nodeCount, leafCount, loads)
	}
	for node, count := range successes {
		if count != 1 {
			t.Errorf("node %s published %d successes", node, count)
		}
	}
	for node, count := range leafCalls {
		if count != 1 {
			t.Errorf("leaf %s executed %d times", node, count)
		}
	}
	mu.Unlock()
	chains, references, summaries := 0, 0, 0
	planKeys := map[string]bool{}
	for runID := firstRunID; runID != ""; {
		chains++
		next, events, historyBytes := "", 0, 0
		activityTypes := map[int64]string{}
		taskBytes := map[int64]int{}
		iter := c.GetWorkflowHistory(ctx, id, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for iter.HasNext() {
			event, err := iter.Next()
			if err != nil {
				t.Fatal(err)
			}
			events++
			historyBytes += proto.Size(event)
			var payloads *commonpb.Payloads
			if started := event.GetWorkflowExecutionStartedEventAttributes(); started != nil {
				payloads = started.Input
			}
			if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil {
				taskBytes[scheduled.WorkflowTaskCompletedEventId] += proto.Size(event)
				activityTypes[event.EventId] = scheduled.ActivityType.Name
				payloads = scheduled.Input
				if scheduled.ActivityType.Name == ExecuteActivity {
					var summary string
					if metadata := event.GetUserMetadata(); metadata != nil && metadata.Summary != nil {
						if err := dc.FromPayload(metadata.Summary, &summary); err != nil {
							t.Fatal(err)
						}
					}
					if !strings.Contains(summary, "model_") || !strings.Contains(summary, "attempt 1") {
						t.Errorf("unhelpful leaf summary: %q", summary)
					}
					summaries++
				}
			}
			if started := event.GetTimerStartedEventAttributes(); started != nil {
				taskBytes[started.WorkflowTaskCompletedEventId] += proto.Size(event)
			}
			if cancelled := event.GetTimerCanceledEventAttributes(); cancelled != nil {
				taskBytes[cancelled.WorkflowTaskCompletedEventId] += proto.Size(event)
			}
			if completed := event.GetActivityTaskCompletedEventAttributes(); completed != nil {
				payloads = completed.Result
				if activityTypes[completed.ScheduledEventId] == PlanActivity && payloads != nil && len(payloads.Payloads) == 1 {
					planKeys[string(payloads.Payloads[0].Data)] = true
				}
			}
			if continued := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); continued != nil {
				next, payloads = continued.NewExecutionRunId, continued.Input
				var input RunInput
				if err := dc.FromPayloads(payloads, &input); err != nil {
					t.Fatal(err)
				}
				if input.Plan.Version != "" || input.Checkpoint == nil {
					t.Fatal("continuation repeated plan or lost checkpoint")
				}
			}
			if payloads != nil {
				for _, payload := range payloads.Payloads {
					if len(payload.Data) > defaultPayloadThreshold {
						t.Fatalf("large inline Temporal payload: %d bytes", len(payload.Data))
					}
					if string(payload.Metadata[converter.MetadataEncoding]) == externalPayloadEncoding {
						references++
						if _, err := blobs.Get(string(payload.Data)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
		if events >= 12000 || historyBytes >= 16<<20 {
			t.Fatalf("unbounded history: %d events, %d bytes", events, historyBytes)
		}
		maxTaskBytes := 0
		for _, bytes := range taskBytes {
			if bytes > maxTaskBytes {
				maxTaskBytes = bytes
			}
		}
		if maxTaskBytes >= 2<<20 {
			t.Fatalf("command event batch has insufficient transaction margin: %d bytes", maxTaskBytes)
		}
		t.Logf("history %s: %d events, %d bytes, largest command event batch %d bytes", runID, events, historyBytes, maxTaskBytes)
		runID = next
	}
	if chains < 2 || references < 6 || len(planKeys) != 1 || summaries != 4 {
		t.Fatalf("chains=%d refs=%d planBlobs=%d summaries=%d", chains, references, len(planKeys), summaries)
	}
	t.Logf("workflow %s: %d histories, %d external refs, one immutable plan blob; retained payloads at %s", id, chains, references, root)
}
