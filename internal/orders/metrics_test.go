package orders_test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/telemetry/telemetrytest"
)

var reader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	reader = telemetrytest.NewReader()
	os.Exit(m.Run())
}

func completed(t *testing.T, status, reason string) int64 {
	t.Helper()
	return telemetrytest.Sum(t, reader, "saga.orders.completed",
		attribute.String("status", status), attribute.String("reason", reason))
}

func TestCreate_CountsCreatedOrders(t *testing.T) {
	e := setup(t)
	before := telemetrytest.Sum(t, reader, "saga.orders.created")
	require.Equal(t, http.StatusAccepted, post(e.svc.Routes(), validBody).Code)
	require.Equal(t, http.StatusBadRequest, post(e.svc.Routes(), `{`).Code)
	require.EqualValues(t, before+1, telemetrytest.Sum(t, reader, "saga.orders.created"))
}

func TestHandleResult_RedeliveryCountsOnce(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	before := completed(t, "confirmed", "")
	durations := telemetrytest.HistogramCount(t, reader, "saga.order.duration")

	env := mustEnv(t, events.PaymentSucceeded, id, events.PaymentSucceededPayload{PaymentID: uuid.New(), AmountCents: 2000})
	require.NoError(t, e.svc.HandleResult(context.Background(), env))
	require.NoError(t, e.svc.HandleResult(context.Background(), env))

	require.EqualValues(t, before+1, completed(t, "confirmed", ""))
	require.EqualValues(t, durations+1, telemetrytest.HistogramCount(t, reader, "saga.order.duration"))
}

func TestHandleResult_CancelReasons(t *testing.T) {
	e := setup(t)
	cases := map[string]events.Envelope{
		events.ReasonOutOfStock:    mustEnv(t, events.InventoryRejected, uuid.Nil, events.InventoryRejectedPayload{Reason: events.ReasonOutOfStock}),
		events.ReasonUnknownSKU:    mustEnv(t, events.InventoryRejected, uuid.Nil, events.InventoryRejectedPayload{Reason: events.ReasonUnknownSKU}),
		events.ReasonPaymentFailed: mustEnv(t, events.PaymentFailed, uuid.Nil, events.PaymentFailedPayload{Reason: events.ReasonDeclined}),
	}
	for reason, env := range cases {
		before := completed(t, "cancelled", reason)
		env.OrderID = e.pending(t)
		require.NoError(t, e.svc.HandleResult(context.Background(), env), reason)
		require.EqualValues(t, before+1, completed(t, "cancelled", reason), reason)
	}
}

func TestStuckPending_CountsOnlyOldPending(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, row := range []struct{ status, age string }{
		{"PENDING", "3 minutes"}, {"PENDING", "10 seconds"}, {"CONFIRMED", "3 minutes"},
	} {
		_, err := e.pool.Exec(ctx,
			`INSERT INTO orders (id, customer_email, items, status, created_at) VALUES ($1, 'a@b.c', '[]', $2, now() - $3::interval)`,
			uuid.New(), row.status, row.age)
		require.NoError(t, err)
	}
	unregister, err := e.svc.RegisterMetrics()
	require.NoError(t, err)
	t.Cleanup(func() { _ = unregister() })

	v, ok := telemetrytest.Gauge(t, reader, "saga.orders.stuck_pending")
	require.True(t, ok)
	require.EqualValues(t, 1, v)
}

func TestStuckPending_DatabaseErrorObservesNothing(t *testing.T) {
	e := setup(t)
	unregister, err := e.svc.RegisterMetrics()
	require.NoError(t, err)
	t.Cleanup(func() { _ = unregister() })
	e.pool.Close()

	_, ok := telemetrytest.Gauge(t, reader, "saga.orders.stuck_pending")
	require.False(t, ok)
}
