package telemetry_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry"
)

func TestSetup_NoEndpointExportsNothingButSpansHaveIDs(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := telemetry.Setup(context.Background(), "test-svc")
	require.NoError(t, err)

	_, span := otel.Tracer("x").Start(context.Background(), "op")
	require.True(t, span.SpanContext().IsValid())
	span.End()

	require.NoError(t, shutdown(context.Background()))
}

func TestSetup_UnreachableCollectorDoesNotBlock(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1") // closed port

	shutdown, err := telemetry.Setup(context.Background(), "test-svc")
	require.NoError(t, err)

	start := time.Now()
	tracer := otel.Tracer("x")
	counter, err := otel.Meter("x").Int64Counter("c")
	require.NoError(t, err)
	for range 1000 {
		_, span := tracer.Start(context.Background(), "op")
		span.End()
	}
	counter.Add(context.Background(), 1)
	require.Less(t, time.Since(start), time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start = time.Now()
	_ = shutdown(ctx) // an error is acceptable; blocking is not
	require.Less(t, time.Since(start), 3*time.Second)
}
