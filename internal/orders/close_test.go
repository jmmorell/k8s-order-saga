package orders_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
)

func (e env) pending(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO orders (id, customer_email, items, status) VALUES ($1, 'a@b.c', '[{"sku":"SKU-001","qty":2}]', 'PENDING')`, id)
	require.NoError(t, err)
	return id
}

func (e env) order(t *testing.T, id uuid.UUID) (status, reason string, amount *int64) {
	t.Helper()
	var r *string
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT status, reason, amount_cents FROM orders WHERE id=$1`, id).Scan(&status, &r, &amount))
	if r != nil {
		reason = *r
	}
	return status, reason, amount
}

func (e env) last(t *testing.T, subject events.Subject) events.Envelope {
	t.Helper()
	s, err := e.js.Stream(context.Background(), "ORDERS")
	require.NoError(t, err)
	raw, err := s.GetLastMsgForSubject(context.Background(), string(subject))
	require.NoError(t, err)
	var env events.Envelope
	require.NoError(t, json.Unmarshal(raw.Data, &env))
	return env
}

func mustEnv(t *testing.T, s events.Subject, id uuid.UUID, p any) events.Envelope {
	t.Helper()
	env, err := events.New(s, id, p)
	require.NoError(t, err)
	return env
}

func TestResult_PaymentSucceededConfirms(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	require.NoError(t, e.svc.HandleResult(context.Background(),
		mustEnv(t, events.PaymentSucceeded, id, events.PaymentSucceededPayload{PaymentID: uuid.New(), AmountCents: 2000})))

	status, _, amount := e.order(t, id)
	require.Equal(t, "CONFIRMED", status)
	require.NotNil(t, amount)
	require.EqualValues(t, 2000, *amount)

	msg := e.last(t, events.OrderConfirmed)
	require.Equal(t, id, msg.OrderID)
	var p events.OrderConfirmedPayload
	require.NoError(t, msg.Decode(&p))
	require.Equal(t, events.OrderConfirmedPayload{CustomerEmail: "a@b.c", AmountCents: 2000}, p)
}

func TestResult_InventoryRejectedCancelsWithReason(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	require.NoError(t, e.svc.HandleResult(context.Background(),
		mustEnv(t, events.InventoryRejected, id, events.InventoryRejectedPayload{Reason: events.ReasonOutOfStock, SKU: "SKU-001"})))

	status, reason, _ := e.order(t, id)
	require.Equal(t, "CANCELLED", status)
	require.Equal(t, "out_of_stock", reason)

	var p events.OrderCancelledPayload
	require.NoError(t, e.last(t, events.OrderCancelled).Decode(&p))
	require.Equal(t, events.OrderCancelledPayload{CustomerEmail: "a@b.c", Reason: "out_of_stock"}, p)
}

func TestResult_PaymentFailedCancels(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	require.NoError(t, e.svc.HandleResult(context.Background(),
		mustEnv(t, events.PaymentFailed, id, events.PaymentFailedPayload{Reason: events.ReasonDeclined})))

	status, reason, _ := e.order(t, id)
	require.Equal(t, "CANCELLED", status)
	require.Equal(t, "payment_failed", reason)
	var p events.OrderCancelledPayload
	require.NoError(t, e.last(t, events.OrderCancelled).Decode(&p))
	require.Equal(t, "payment_failed", p.Reason)
}

func TestResult_DuplicateDeliveryPublishesOnce(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	env := mustEnv(t, events.PaymentSucceeded, id, events.PaymentSucceededPayload{PaymentID: uuid.New(), AmountCents: 2000})
	require.NoError(t, e.svc.HandleResult(context.Background(), env))
	require.NoError(t, e.svc.HandleResult(context.Background(), env))
	require.EqualValues(t, 1, e.streamMsgs(t, "ORDERS"))
}

func TestResult_LateResultDoesNotChangeClosedOrder(t *testing.T) {
	e := setup(t)
	id := e.pending(t)
	ctx := context.Background()
	require.NoError(t, e.svc.HandleResult(ctx,
		mustEnv(t, events.PaymentFailed, id, events.PaymentFailedPayload{Reason: events.ReasonDeclined})))
	require.NoError(t, e.svc.HandleResult(ctx,
		mustEnv(t, events.PaymentSucceeded, id, events.PaymentSucceededPayload{AmountCents: 1})))

	status, reason, amount := e.order(t, id)
	require.Equal(t, "CANCELLED", status)
	require.Equal(t, "payment_failed", reason)
	require.Nil(t, amount)
}

func TestResult_UnknownOrderIsPermanent(t *testing.T) {
	e := setup(t)
	err := e.svc.HandleResult(context.Background(),
		mustEnv(t, events.PaymentFailed, uuid.New(), events.PaymentFailedPayload{Reason: events.ReasonDeclined}))
	require.Error(t, err)
	require.True(t, errors.Is(err, events.ErrPermanent))
}
