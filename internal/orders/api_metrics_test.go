package orders

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The request Host header is client-controlled, so it must never become a
// metric label: otelhttp's server metrics would otherwise mint one
// server.address series per distinct Host.
func TestTraced_ServerAddressIsBounded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	h := traced(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, host := range []string{"a.example", "b.example", "c.example:9999"} {
		req := httptest.NewRequest(http.MethodGet, "/orders/x", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	addrs := map[string]bool{}
	seen := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			seen++
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("server.address")); ok {
					addrs[v.AsString()] = true
				}
			}
		}
	}
	require.NotZero(t, seen, "otelhttp server duration metric not found")
	require.Len(t, addrs, 1, "server.address must be constant, got %v", addrs)
}
