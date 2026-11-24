package events

import (
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/jmmorell/k8s-order-saga/internal/platform/events"

func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// headerCarrier lets the propagator read and write trace context in NATS headers.
type headerCarrier nats.Header

func (c headerCarrier) Get(key string) string { return nats.Header(c).Get(key) }

func (c headerCarrier) Set(key, value string) { nats.Header(c).Set(key, value) }

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

func messagingAttrs(subject string, e Envelope) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("messaging.system", "nats"),
		attribute.String("messaging.destination.name", subject),
		attribute.String("messaging.message.id", e.ID.String()),
		attribute.String("order.id", e.OrderID.String()),
	}
}
