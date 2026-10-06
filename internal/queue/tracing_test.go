package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/protocol"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/store/db"
)

func TestRuntimeDiagnosticsKeepIdentityWithoutPrivateData(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(ctx)
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	schema, err := db.New(r.Store.Pool).CurrentSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.Client, err = New(r.Store.Pool, Config{EngineID: r.Store.EngineID, HostID: r.HostID, Schema: schema,
		Logger: log, ExecutionWorkers: 2, ExecutionTimeout: time.Minute}, r.Handlers())
	if err != nil {
		t.Fatal(err)
	}
	const input = "private-input-sentinel"
	const prompt = "private-prompt-sentinel"
	const answer = "private-output-sentinel"
	const reasoning = "private-reasoning-sentinel"
	const dbError = "private-database-error-sentinel"
	secret := "private-credential-sentinel"
	content, _ := json.Marshal(map[string]string{"answer": answer})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil || !bytes.Contains(body, []byte(input)) || !bytes.Contains(body, []byte(prompt)) || req.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("physical provider request lost its private input, prompt or authentication")
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{
			"role": "assistant", "content": string(content), "thinking": reasoning}, "done": true, "done_reason": "stop"})
	}))
	defer server.Close()
	plan := runtimePlan(server.URL)
	connection := plan.Profile.Spec.Models["local"]
	connection.Auth = map[string]contract.Credential{"key": {Value: &secret}}
	plan.Profile.Spec.Models["local"] = connection
	definition := &plan.Pipelines[plan.Root].Spec
	port := contract.Port{Schema: json.RawMessage(`{"type":"string"}`)}
	definition.Inputs = map[string]contract.Port{"private": port}
	for id, node := range definition.Nodes {
		// runtimePlan shares the LLM pointer; assign a fresh prompt for each node.
		node.LLM = &contract.LLMNode{Model: "model", Prompt: contract.TextSource{Text: prompt}}
		if node.Inputs == nil {
			node.Inputs = map[string]contract.Port{}
		}
		binding := port
		binding.Bind = &contract.Binding{From: "inputs.private"}
		node.Inputs["private"] = binding
		definition.Nodes[id] = node
	}
	// A sequence is intentionally outside transaction rollback: fail exactly
	// one real publication with private PostgreSQL error text, then recover.
	if _, err := r.Store.Pool.Exec(ctx, `CREATE SEQUENCE diagnostics_fault;
		CREATE FUNCTION diagnostics_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF nextval('diagnostics_fault')=1 THEN RAISE EXCEPTION 'private-database-error-sentinel'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER diagnostics_fault BEFORE UPDATE ON knotra_execution_attempts
		FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL) EXECUTE FUNCTION diagnostics_fault()`); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	encodedInput, _ := json.Marshal(input)
	id := admitRuntimeRun(t, r, plan, contract.Values{"private": {JSON: encodedInput}})
	await(t, 20*time.Second, func() (bool, error) {
		status, err := store.ReadRunStatus(ctx, r.Store.Pool, id)
		return protocol.Terminal(status), err
	})
	run, err := r.Store.Run(ctx, id)
	if err != nil || run.Status != "succeeded" || calls.Load() != 3 {
		t.Fatalf("status=%s calls=%d error=%v", run.Status, calls.Load(), err)
	}
	var output string
	if err := json.Unmarshal(run.Outputs["result"], &output); err != nil || output != answer {
		t.Fatal("diagnostics changed the actual private output")
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	jobs, err := r.Client.River.JobList(ctx, river.NewJobListParams().First(100))
	if err != nil {
		t.Fatal(err)
	}
	failedJobs := 0
	for _, job := range jobs.Jobs {
		failedJobs += len(job.Errors)
		for _, problem := range job.Errors {
			if !strings.HasPrefix(problem.Error, "execution failed (") {
				t.Fatalf("River persisted unsanitized error: %s", problem.Error)
			}
		}
	}
	if failedJobs == 0 {
		t.Fatal("publication fault never reached the real River error journal")
	}
	spans := tracetest.SpanStubsFromReadOnlySpans(recorder.Ended())
	nodes, publications, recoveries, executions := 0, 0, 0, 0
	operationEvents := map[string]int{}
	for _, span := range spans {
		attrs := attribute.NewSet(span.Attributes...)
		if span.Name == "river.knotra_execute_v1" {
			if _, claimed := attrs.Value("knotra.ownership.generation"); !claimed {
				continue // Capacity/stale delivery completes without claiming an owner.
			}
			executions++
		}
		switch span.Name {
		case "node.llm", "outcome.publish", "river.knotra_execute_v1":
			for _, key := range []attribute.Key{"knotra.run.id", "knotra.instance.id", "knotra.attempt", "knotra.worker.id", "knotra.ownership.generation", "knotra.engine.id", "knotra.host.id"} {
				value, found := attrs.Value(key)
				if !found || value.AsInterface() == "" {
					t.Fatalf("%s lacks %s", span.Name, key)
				}
			}
			runID, _ := attrs.Value("knotra.run.id")
			workerID, _ := attrs.Value("knotra.worker.id")
			generation, _ := attrs.Value("knotra.ownership.generation")
			if runID.AsString() != id || workerID.AsString() != r.WorkerID || generation.AsInt64() < 1 {
				t.Fatalf("%s has incorrect ownership", span.Name)
			}
		}
		if span.Name == "node.llm" {
			nodes++
			if !span.Parent.IsValid() {
				t.Fatal("node lost its delivery parent")
			}
		}
		if span.Name == "outcome.publish" {
			publications++
			value, _ := attrs.Value("recovery")
			if value.AsBool() {
				recoveries++
			}
		}
		for _, event := range span.Events {
			if strings.HasPrefix(event.Name, "operation.") {
				eventAttrs := attribute.NewSet(event.Attributes...)
				operationID, found := eventAttrs.Value("knotra.operation.id")
				if !found || operationID.AsString() == "" {
					t.Fatal("journal boundary lost its operation identity")
				}
				operationEvents[event.Name]++
			}
		}
	}
	if nodes != 3 || executions != 3 || publications < 4 || recoveries < 1 || operationEvents["operation.intent"] != 3 || operationEvents["operation.admission"] != 3 || operationEvents["operation.confirmation"] != 3 {
		t.Fatalf("nodes=%d executions=%d publications=%d recoveries=%d operation events=%v", nodes, executions, publications, recoveries, operationEvents)
	}
	for _, identity := range []string{id, r.WorkerID, r.Store.EngineID, r.HostID, "generation", "attempt", "knotra.operation.id", "outcome.publication"} {
		if !strings.Contains(logs.String(), identity) {
			t.Fatalf("logs lack identity/boundary %s", identity)
		}
	}
	encodedSpans, _ := json.Marshal(spans)
	encodedJobs, _ := json.Marshal(jobs.Jobs)
	diagnostics := logs.String() + string(encodedSpans) + string(encodedJobs)
	for _, private := range []string{input, prompt, answer, reasoning, secret, dbError, server.URL} {
		if strings.Contains(diagnostics, private) {
			t.Fatal("private execution data leaked into diagnostics or River history")
		}
	}
}

func TestDeliveryRedactionPreservesRiverControlErrorsAndPanics(t *testing.T) {
	private := errors.New("private-error-sentinel")
	var logs bytes.Buffer
	client := &Client{logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	job := &river.Job[ExecuteArgs]{JobRow: &rivertype.JobRow{ID: 1, Queue: "fixture"}, Args: ExecuteArgs{
		Attempt: execution.AttemptID{RunID: "run", InstanceID: "instance", Number: 1}, DispatchGeneration: 1, RoutingVersion: RoutingVersion}}
	for _, original := range []error{private, river.JobCancel(private), river.JobSnooze(time.Minute), context.Canceled} {
		worker := &deliveryWorker[ExecuteArgs]{client: client, work: func(context.Context, *river.Job[ExecuteArgs]) error { return original }}
		err := worker.Work(context.Background(), job)
		if !errors.Is(err, original) || strings.Contains(err.Error(), private.Error()) {
			t.Fatal("redaction lost error identity or exposed private error text")
		}
		var cancel *river.JobCancelError
		var snooze *river.JobSnoozeError
		if errors.As(original, &cancel) && !errors.As(err, &cancel) || errors.As(original, &snooze) && !errors.As(err, &snooze) {
			t.Fatal("redaction changed River cancellation/snooze semantics")
		}
	}
	worker := &deliveryWorker[ExecuteArgs]{client: client, work: func(context.Context, *river.Job[ExecuteArgs]) error { panic(private) }}
	func() {
		defer func() {
			value := recover()
			if value == nil || strings.Contains(fmt.Sprint(value), private.Error()) {
				t.Fatal("panic handling disappeared or exposed private value")
			}
		}()
		_ = worker.Work(context.Background(), job)
	}()
	if strings.Contains(logs.String(), private.Error()) {
		t.Fatal("private error text leaked into delivery logs")
	}
}
