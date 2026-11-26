package inventory

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instrument comes from the global meter; the global provider hands it
// to the real provider once telemetry setup installs it.
var operationsCounter = func() metric.Int64Counter {
	c, err := otel.Meter("github.com/jmmorell/k8s-order-saga/internal/inventory").
		Int64Counter("saga.inventory.operations", metric.WithDescription("Stock operations by result, counted once per order."))
	if err != nil {
		panic(err) // only fails on an invalid instrument name
	}
	return c
}()

func recordOperation(ctx context.Context, result string) {
	operationsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
