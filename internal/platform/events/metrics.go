package events

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Consume outcomes recorded on saga.events.consumed.
const (
	resultAck        = "ack"
	resultRetry      = "retry"
	resultDeadLetter = "dead_letter"
)

// The instruments come from the global meter; the global provider hands them
// to the real provider once telemetry setup installs it.
var (
	meter = otel.Meter("github.com/jmmorell/k8s-order-saga/internal/platform/events")

	publishedCounter = mustCounter("saga.events.published", "Events stored in JetStream.")
	consumedCounter  = mustCounter("saga.events.consumed", "Events handled by a durable consumer, by outcome.")
)

func mustCounter(name, desc string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(desc))
	if err != nil {
		panic(err) // only fails on an invalid instrument name
	}
	return c
}

func recordPublished(ctx context.Context, subject string) {
	publishedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("subject", subject)))
}

func recordConsumed(ctx context.Context, durable, subject, result string) {
	consumedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("durable", durable),
		attribute.String("subject", subject),
		attribute.String("result", result)))
}
