// Package telemetry provides opt-in OTLP traces and metrics. It never records
// prompts, port values, request bodies, authentication or provider credentials.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Start exports only when the operator explicitly configures an OTLP endpoint.
// The SDK handles OTEL_EXPORTER_OTLP_* settings including transport credentials.
func Start(ctx context.Context, version string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return noop, nil
	}
	traces := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	metrics := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
	if !traces && !metrics {
		return noop, nil
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithAttributes(attribute.String("service.name", "knotra"), attribute.String("service.version", version)))
	if err != nil {
		return nil, err
	}
	shutdowns := []func(context.Context) error{}
	stop := func(ctx context.Context) error {
		var errs []error
		for _, stop := range shutdowns {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}
	if traces {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		otel.SetTracerProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	if metrics {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			_ = stop(ctx)
			return nil, err
		}
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res))
		otel.SetMeterProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return stop, nil
}
func HTTP(next http.Handler) http.Handler {
	meter := otel.Meter("knotra/api")
	requests, _ := meter.Int64Counter("knotra.http.requests")
	duration, _ := meter.Float64Histogram("knotra.http.duration", metric.WithUnit("s"))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer("knotra/api").Start(ctx, "http "+r.Method)
		defer span.End()
		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
		// Pattern contains route templates, never user-controlled IDs or query values.
		attrs := []attribute.KeyValue{attribute.String("http.request.method", r.Method), attribute.String("http.route", r.Pattern)}
		span.SetAttributes(attrs...)
		requests.Add(ctx, 1, metric.WithAttributes(attrs...))
		duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	})
}
