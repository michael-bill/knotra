package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/michael-bill/knotra/internal/execution"
)

func OwnershipAttributes(owner execution.Ownership) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("knotra.run.id", owner.RunID),
		attribute.String("knotra.instance.id", owner.InstanceID), attribute.Int("knotra.attempt", owner.Number),
		attribute.String("knotra.worker.id", owner.WorkerID), attribute.Int64("knotra.ownership.generation", owner.Generation)}
}

func OwnershipLogger(log *slog.Logger, owner execution.Ownership) *slog.Logger {
	if log == nil {
		return nil
	}
	return log.With("runId", owner.RunID, "instanceId", owner.InstanceID, "attempt", owner.Number,
		"workerId", owner.WorkerID, "generation", owner.Generation)
}

// Event records execution boundaries, never arguments, responses or error text.
func Event(ctx context.Context, log *slog.Logger, phase string, err error, attrs ...attribute.KeyValue) {
	attrs = append(attrs, attribute.Bool("failed", err != nil), attribute.String("errorType", fmt.Sprintf("%T", err)))
	trace.SpanFromContext(ctx).AddEvent(phase, trace.WithAttributes(attrs...))
	if log != nil {
		level := slog.LevelDebug
		if err != nil {
			level = slog.LevelWarn
		}
		logAttrs := make([]any, 0, len(attrs))
		for _, attr := range attrs {
			logAttrs = append(logAttrs, slog.Any(string(attr.Key), attr.Value.AsInterface()))
		}
		log.Log(ctx, level, phase, logAttrs...)
	}
}

// RedactError keeps error identity/control semantics through Unwrap, but prevents
// SDK/background logs and queue error history from exposing payloads or URLs.
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	return privateError{cause: err}
}

type privateError struct{ cause error }

func (e privateError) Error() string { return fmt.Sprintf("execution failed (%T)", e.cause) }
func (e privateError) Unwrap() error { return e.cause }
