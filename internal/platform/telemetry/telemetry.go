// Package telemetry wires up OpenTelemetry traces, metrics and logs for a service.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const endpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Setup installs the global propagator, tracer provider and, when
// OTEL_EXPORTER_OTLP_ENDPOINT is set, OTLP/HTTP exporters for traces, metrics
// and logs. Without an endpoint nothing is exported and nothing connects, but
// spans still get valid IDs so logs can be correlated. The returned shutdown
// flushes and stops everything Setup created.
func Setup(ctx context.Context, service string) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", service)),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	var shutdowns []func(context.Context) error
	shutdownAll := func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}

	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	exporting := os.Getenv(endpointEnv) != ""
	if exporting {
		te, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("trace exporter: %w", err)
		}
		traceOpts = append(traceOpts, sdktrace.WithBatcher(te))
	}
	tp := sdktrace.NewTracerProvider(traceOpts...)
	otel.SetTracerProvider(tp)
	shutdowns = append(shutdowns, tp.Shutdown)

	if !exporting {
		return shutdownAll, nil
	}

	me, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = shutdownAll(ctx)
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me)),
	)
	otel.SetMeterProvider(mp)
	shutdowns = append(shutdowns, mp.Shutdown)

	le, err := otlploghttp.New(ctx)
	if err != nil {
		_ = shutdownAll(ctx)
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(le)),
	)
	otel.SetLoggerProvider(lp)
	shutdowns = append(shutdowns, lp.Shutdown)

	return shutdownAll, nil
}
