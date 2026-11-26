package telemetrytest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry/telemetrytest"
)

func TestHelpers_ReadWhatInstrumentsRecord(t *testing.T) {
	r := telemetrytest.NewReader()
	m := otel.Meter("telemetrytest")
	ctx := context.Background()

	c, err := m.Int64Counter("t.counter")
	require.NoError(t, err)
	c.Add(ctx, 2, metric.WithAttributes(attribute.String("k", "a")))
	c.Add(ctx, 3, metric.WithAttributes(attribute.String("k", "b")))
	require.EqualValues(t, 2, telemetrytest.Sum(t, r, "t.counter", attribute.String("k", "a")))
	require.EqualValues(t, 0, telemetrytest.Sum(t, r, "t.counter", attribute.String("k", "c")))
	require.EqualValues(t, 0, telemetrytest.Sum(t, r, "t.missing"))

	h, err := m.Float64Histogram("t.hist")
	require.NoError(t, err)
	h.Record(ctx, 1)
	h.Record(ctx, 2, metric.WithAttributes(attribute.String("k", "a")))
	require.EqualValues(t, 2, telemetrytest.HistogramCount(t, r, "t.hist"))

	_, ok := telemetrytest.Gauge(t, r, "t.gauge")
	require.False(t, ok)
	_, err = m.Int64ObservableGauge("t.gauge", metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(7)
		return nil
	}))
	require.NoError(t, err)
	v, ok := telemetrytest.Gauge(t, r, "t.gauge")
	require.True(t, ok)
	require.EqualValues(t, 7, v)
}
