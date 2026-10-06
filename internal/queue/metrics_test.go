package queue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/execution"
	"github.com/michael-bill/knotra/internal/store"
	"github.com/michael-bill/knotra/internal/telemetry"
)

func TestSchedulerMetricsCollectDurableBacklogAndFailures(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(ctx)
	})
	registration, err := r.observeMetrics()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Unregister() })
	collect := func() map[string]float64 {
		t.Helper()
		var result metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &result); err != nil {
			t.Fatal(err)
		}
		values := map[string]float64{}
		for _, scope := range result.ScopeMetrics {
			for _, measurement := range scope.Metrics {
				switch data := measurement.Data.(type) {
				case metricdata.Gauge[float64]:
					for _, point := range data.DataPoints {
						if point.Attributes.Len() != 0 {
							t.Fatal("scheduler metrics must not label execution data or identities")
						}
						values[measurement.Name] = point.Value
					}
				case metricdata.Sum[int64]:
					for _, point := range data.DataPoints {
						values[measurement.Name] = float64(point.Value)
					}
				}
			}
		}
		return values
	}
	for name, value := range collect() {
		if value != 0 {
			t.Fatalf("idle %s=%v", name, value)
		}
	}
	plan := runtimePlan("")
	plan.Pipelines[plan.Root].Spec.Nodes = map[string]contract.Node{"a": plan.Pipelines[plan.Root].Spec.Nodes["a"]}
	plan.Pipelines[plan.Root].Spec.Outputs = nil
	id := admitRuntimeRun(t, r, plan)
	if _, err := r.Store.Pool.Exec(ctx, `UPDATE knotra_runs SET dirty_since=clock_timestamp()-interval '1 minute' WHERE id=$1;
		UPDATE river_job SET scheduled_at=clock_timestamp()-interval '40 seconds'`, pgx.QueryExecModeSimpleProtocol, id); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		_, err := store.WakeExecution(ctx, tx, id, r.Wake)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	values := collect()
	if values["knotra.scheduler.dirty_run.age"] < 60 || values["knotra.scheduler.ready_job.age"] < 40 {
		t.Fatalf("coalesced backlog ages=%v", values)
	}
	if _, err := r.Store.Pool.Exec(ctx, "UPDATE river_job SET scheduled_at=clock_timestamp()+interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if value := collect()["knotra.scheduler.ready_job.age"]; value != 0 {
		t.Fatalf("future job age=%v", value)
	}
	if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID); err != nil {
		t.Fatal(err)
	}
	var claim *execution.Claim
	if err := pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		if _, err := store.ApplyWake(ctx, tx, id, 2); err != nil {
			return err
		}
		if err := r.advanceTx(ctx, tx, plan, id, ""); err != nil {
			return err
		}
		var err error
		claim, err = r.Store.ClaimAttempt(ctx, tx, execution.AttemptID{RunID: id, InstanceID: execution.StableID("n", id+"/root/a"), Number: 1}, 1, r.WorkerID)
		return err
	}); err != nil || claim == nil {
		t.Fatalf("claim=%+v error=%v", claim, err)
	}
	directory := t.TempDir()
	files, err := execution.OpenOutcomeFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	r.Outcomes = files
	outcome := execution.Outcome{FormatVersion: execution.StateFormatVersion, Ownership: claim.Ownership, PlanID: plan.Digest,
		CompletedAt: time.Now(), Outputs: contract.Values{"answer": {JSON: []byte(`"saved"`)}}}
	key, err := files.Put(outcome)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(directory, key+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("publication transaction rolled back")
	if err := r.publish(ctx, outcome, false, func(pgx.Tx) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("publication failure=%v", err)
	}
	if _, err := r.Store.Pool.Exec(ctx, `UPDATE knotra_execution_attempts SET lease_expires_at=clock_timestamp()-interval '1 minute' WHERE run_id=$1;
		UPDATE knotra_execution_nodes SET state='waiting_resolution' WHERE run_id=$1;
		UPDATE knotra_execution_timers SET due_at=clock_timestamp()-interval '1 minute' WHERE run_id=$1`, pgx.QueryExecModeSimpleProtocol, id); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanup(ctx, &river.Job[CleanupArgs]{Args: CleanupArgs{HostID: "another-host"}}); err == nil {
		t.Fatal("wrong-host cleanup succeeded")
	}
	values = collect()
	for _, name := range []string{"lease.expired", "outcome.unknown", "slot.reservations", "outcome.pending", "publication.failures", "cleanup.failures"} {
		if values["knotra.scheduler."+name] != 1 {
			t.Fatalf("%s not observed: %v", name, values)
		}
	}
	if values["knotra.scheduler.outcome.pending.age"] < 60 || values["knotra.scheduler.timer.lag"] < 60 || values["knotra.scheduler.steps"] < 1 {
		t.Fatalf("durable evidence/timer/step metrics=%v", values)
	}
	if _, err := r.Store.Pool.Exec(ctx, `UPDATE knotra_runs SET document=jsonb_set(document::jsonb,'{status}','"cancelled"'),dirty_since=clock_timestamp()-interval '1 day' WHERE id=$1;
		UPDATE knotra_execution_attempts SET outcome='{}',state='completed' WHERE run_id=$1;
		UPDATE knotra_execution_slots SET released_at=clock_timestamp() WHERE run_id=$1;
		UPDATE knotra_execution_nodes SET state='cancelled' WHERE run_id=$1;
		UPDATE river_job SET queue='unrelated',scheduled_at=clock_timestamp()-interval '1 day'`, pgx.QueryExecModeSimpleProtocol, id); err != nil {
		t.Fatal(err)
	}
	values = collect()
	for _, name := range []string{"dirty_run.age", "ready_job.age", "lease.expired", "outcome.unknown", "slot.reservations", "timer.lag", "outcome.pending.age", "outcome.pending"} {
		if values["knotra.scheduler."+name] != 0 {
			t.Fatalf("settled %s=%v", name, values)
		}
	}
	r.Store.Close()
	var result metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &result); err == nil {
		t.Fatal("database outage exported false healthy zeros")
	}
}

func TestSchedulerMetricsExportThroughConfiguredOTLP(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	payloads := make(chan []byte, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil || request.Method != http.MethodPost || request.URL.Path != "/v1/metrics" {
			t.Errorf("OTLP request=%s %s error=%v", request.Method, request.URL.Path, err)
		}
		select {
		case payloads <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")
	previous := otel.GetMeterProvider()
	defer otel.SetMeterProvider(previous)
	stop, err := telemetry.Start(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(ctx) }()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	provider, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider)
	if !ok {
		t.Fatal("OTLP meter provider was not configured")
	}
	if err := provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-payloads:
		for _, name := range []string{"dirty_run.age", "ready_job.age", "lease.expired", "outcome.unknown", "slot.reservations", "timer.lag", "outcome.pending.age", "outcome.pending"} {
			if !bytes.Contains(body, []byte("knotra.scheduler."+name)) {
				t.Fatalf("OTLP payload omits %s", name)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OTLP collector received no scheduler metrics")
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
