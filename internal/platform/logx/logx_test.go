package logx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
)

func TestParseLevel(t *testing.T) {
	require.Equal(t, slog.LevelDebug, logx.ParseLevel("debug"))
	require.Equal(t, slog.LevelWarn, logx.ParseLevel("WARN"))
	require.Equal(t, slog.LevelError, logx.ParseLevel("error"))
	require.Equal(t, slog.LevelInfo, logx.ParseLevel("info"))
	require.Equal(t, slog.LevelInfo, logx.ParseLevel("bogus"))
}

func TestNewWithWriter_JSONWithServiceAttr(t *testing.T) {
	var buf bytes.Buffer
	logx.NewWithWriter(&buf, "orders", "info").Info("hello")
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	require.Equal(t, "orders", m["service"])
	require.Equal(t, "hello", m["msg"])
}

func TestLogger_AddsTraceIDInsideSpan(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	log := logx.NewWithWriter(&buf, "svc", "info")
	log.InfoContext(ctx, "inside")
	log.InfoContext(context.Background(), "outside")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.Len(t, lines, 2)
	var in, out map[string]any
	require.NoError(t, json.Unmarshal(lines[0], &in))
	require.NoError(t, json.Unmarshal(lines[1], &out))
	require.Equal(t, span.SpanContext().TraceID().String(), in["trace_id"])
	require.Equal(t, span.SpanContext().SpanID().String(), in["span_id"])
	require.NotContains(t, out, "trace_id")
	require.NotContains(t, out, "span_id")
}

func TestLogger_LevelFiltersBridgeToo(t *testing.T) {
	var buf bytes.Buffer
	log := logx.NewWithWriter(&buf, "svc", "warn")
	log.Info("dropped")
	require.Empty(t, buf.String())
	require.False(t, log.Enabled(context.Background(), slog.LevelInfo))
}
