// Package logx builds the JSON slog logger shared by all services.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// New returns a JSON logger writing to stdout with a "service" attribute on every line.
// level is one of debug, info, warn, error; anything else means info.
// Records also go to the global OpenTelemetry logger provider (a no-op unless
// telemetry.Setup installed one), and lines logged with a context that carries a
// span get trace_id and span_id attributes.
func New(service, level string) *slog.Logger {
	return NewWithWriter(os.Stdout, service, level)
}

// NewWithWriter is like New but writes to w.
func NewWithWriter(w io.Writer, service, level string) *slog.Logger {
	lvl := ParseLevel(level)
	stdout := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	h := &leveled{
		level: lvl,
		next:  slog.NewMultiHandler(traced{stdout}, otelslog.NewHandler(service)),
	}
	return slog.New(h).With("service", service)
}

// leveled drops records below level for every handler it fans out to.
type leveled struct {
	level slog.Level
	next  slog.Handler
}

func (l *leveled) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= l.level && l.next.Enabled(ctx, lv)
}

func (l *leveled) Handle(ctx context.Context, r slog.Record) error {
	return l.next.Handle(ctx, r)
}

func (l *leveled) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &leveled{level: l.level, next: l.next.WithAttrs(attrs)}
}

func (l *leveled) WithGroup(name string) slog.Handler {
	return &leveled{level: l.level, next: l.next.WithGroup(name)}
}

// traced adds trace_id and span_id when the record's context carries a valid span.
type traced struct{ slog.Handler }

func (t traced) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return t.Handler.Handle(ctx, r)
}

func (t traced) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traced{t.Handler.WithAttrs(attrs)}
}

func (t traced) WithGroup(name string) slog.Handler {
	return traced{t.Handler.WithGroup(name)}
}

// ParseLevel maps a level name (case-insensitive) to a slog level, defaulting to info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
