// Package telemetrytest helps tests assert on OpenTelemetry metrics.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// NewReader installs a global MeterProvider backed by the returned reader.
// Call it from TestMain, before any instrument is created.
func NewReader() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
}

// Sum returns the current value of the int64 counter series called name whose
// attributes are exactly attrs, or 0 if there is none.
func Sum(t testing.TB, r *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	want := attribute.NewSet(attrs...)
	m := find(t, r, name)
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is %T, not an int64 sum", name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

// HistogramCount returns the number of observations recorded in the histogram
// called name, across all attribute sets.
func HistogramCount(t testing.TB, r *sdkmetric.ManualReader, name string) uint64 {
	t.Helper()
	m := find(t, r, name)
	if m == nil {
		return 0
	}
	var n uint64
	switch h := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range h.DataPoints {
			n += dp.Count
		}
	case metricdata.Histogram[int64]:
		for _, dp := range h.DataPoints {
			n += dp.Count
		}
	default:
		t.Fatalf("metric %q is %T, not a histogram", name, m.Data)
	}
	return n
}

func find(t testing.TB, r *sdkmetric.ManualReader, name string) *metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}
