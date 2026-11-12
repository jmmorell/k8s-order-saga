package logx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/platform/logx"
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
