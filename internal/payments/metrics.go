package payments

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instrument comes from the global meter; the global provider hands it
// to the real provider once telemetry setup installs it.
var paymentsCounter = func() metric.Int64Counter {
	c, err := otel.Meter("github.com/jmmorell/k8s-order-saga/internal/payments").
		Int64Counter("saga.payments", metric.WithDescription("Charges by result, counted once per order."))
	if err != nil {
		panic(err) // only fails on an invalid instrument name
	}
	return c
}()

func recordPayment(ctx context.Context, result string) {
	paymentsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
