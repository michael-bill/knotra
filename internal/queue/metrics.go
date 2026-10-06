package queue

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/michael-bill/knotra/internal/store/db"
)

// Metrics are collected only by the configured OTLP reader, not the one-second
// maintenance loop. Failed reads produce a collection error, never false zeros.
func (r *Runtime) observeMetrics() (metric.Registration, error) {
	meter := otel.Meter("knotra/scheduler")
	names := []struct{ name, unit string }{
		{"dirty_run.age", "s"}, {"ready_job.age", "s"},
		{"lease.expired", "{attempt}"}, {"outcome.unknown", "{instance}"},
		{"slot.reservations", "{reservation}"}, {"timer.lag", "s"},
		{"outcome.pending.age", "s"}, {"outcome.pending", "{outcome}"},
	}
	gauges := make([]metric.Float64ObservableGauge, len(names))
	instruments := make([]metric.Observable, len(names))
	for i, name := range names {
		gauge, err := meter.Float64ObservableGauge("knotra.scheduler."+name.name, metric.WithUnit(name.unit))
		if err != nil {
			return nil, err
		}
		gauges[i], instruments[i] = gauge, gauge
	}
	return meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		snapshot, err := db.New(r.Store.Pool).ReadSchedulerMetrics(ctx)
		if err != nil {
			return err
		}
		jobs, err := r.Client.River.JobList(ctx, river.NewJobListParams().First(1).
			Queues(r.Client.advance, r.Client.execute, r.Client.maintenance, r.Client.cleanup).
			States(rivertype.JobStateAvailable, rivertype.JobStateRetryable, rivertype.JobStateScheduled).
			OrderBy(river.JobListOrderByScheduledAt, river.SortOrderAsc))
		if err != nil {
			return err
		}
		readyAge := 0.0
		if len(jobs.Jobs) > 0 {
			readyAge = max(0, snapshot.ObservedAt.Sub(jobs.Jobs[0].ScheduledAt).Seconds())
		}
		pendingAge, pending := 0.0, 0.0
		// ponytail: stat unimported outcome keys once per export; add an indexed
		// storage notification feed if pending evidence dominates collection time.
		for _, key := range snapshot.PendingKeys {
			if err := ctx.Err(); err != nil {
				return err
			}
			saved, err := r.Outcomes.SavedAt(key)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			pending++
			pendingAge = max(pendingAge, snapshot.ObservedAt.Sub(saved).Seconds())
		}
		values := []float64{snapshot.DirtyAge, readyAge, float64(snapshot.ExpiredLeases), float64(snapshot.UnknownOutcomes),
			float64(snapshot.SlotReservations), snapshot.TimerLag, pendingAge, pending}
		for i, value := range values {
			observer.ObserveFloat64(gauges[i], value)
		}
		return nil
	}, instruments...)
}

func schedulerCount(ctx context.Context, name string) {
	counter, _ := otel.Meter("knotra/scheduler").Int64Counter("knotra.scheduler." + name)
	counter.Add(ctx, 1)
}
