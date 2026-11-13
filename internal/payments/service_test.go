package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/payments"
	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
	"github.com/jmmorell/k8s-order-saga/internal/platform/testinfra"
)

func TestConfig_RejectsFailureRateOutOfRange(t *testing.T) {
	require.Error(t, payments.Config{FailureRate: 1.5}.Validate())
	require.Error(t, payments.Config{FailureRate: -0.1}.Validate())
	require.NoError(t, payments.Config{FailureRate: 0}.Validate())
	require.NoError(t, payments.Config{FailureRate: 1}.Validate())
}

func TestConfig_RejectsNegativeLatency(t *testing.T) {
	require.Error(t, payments.Config{Latency: -time.Millisecond}.Validate())
	require.NoError(t, payments.Config{Latency: 0}.Validate())
}

type env struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
}

func setup(t *testing.T) env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, err := pgutil.Connect(ctx, testinfra.Postgres(t, "payments")["payments"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pgutil.Migrate(ctx, pool, payments.Migrations()))

	nc, js, err := events.Connect(testinfra.NATS(t), "test")
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	require.NoError(t, events.EnsureStreams(ctx, js))
	return env{pool: pool, js: js}
}

func (e env) service(rate float64) *payments.Service {
	return payments.NewService(e.pool, e.js, payments.Config{FailureRate: rate}, slog.New(slog.DiscardHandler))
}

func reserved(t *testing.T, orderID uuid.UUID, amount int64) events.Envelope {
	t.Helper()
	env, err := events.New(events.InventoryReserved, orderID, events.InventoryReservedPayload{
		Items: []events.Item{{SKU: "SKU-001", Qty: 2}}, AmountCents: amount,
	})
	require.NoError(t, err)
	return env
}

func (e env) payment(t *testing.T, orderID uuid.UUID) (status string, amount int64) {
	t.Helper()
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT status, amount_cents FROM payments WHERE order_id=$1`, orderID).Scan(&status, &amount))
	return status, amount
}

func (e env) paymentCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments`).Scan(&n))
	return n
}

// published returns the number of messages in PAYMENTS and the last envelope on subject.
func (e env) published(t *testing.T, subject events.Subject) (total uint64, last events.Envelope) {
	t.Helper()
	ctx := context.Background()
	s, err := e.js.Stream(ctx, "PAYMENTS")
	require.NoError(t, err)
	info, err := s.Info(ctx)
	require.NoError(t, err)
	if info.State.Msgs == 0 {
		return 0, events.Envelope{}
	}
	raw, err := s.GetLastMsgForSubject(ctx, string(subject))
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return info.State.Msgs, events.Envelope{}
	}
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw.Data, &last))
	return info.State.Msgs, last
}

func TestReserved_RateZeroSucceeds(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	require.NoError(t, e.service(0).HandleInventoryReserved(context.Background(), reserved(t, oid, 2000)))

	status, amount := e.payment(t, oid)
	require.Equal(t, "SUCCEEDED", status)
	require.EqualValues(t, 2000, amount)

	total, msg := e.published(t, events.PaymentSucceeded)
	require.EqualValues(t, 1, total)
	require.Equal(t, oid, msg.OrderID)
	var p events.PaymentSucceededPayload
	require.NoError(t, msg.Decode(&p))
	require.EqualValues(t, 2000, p.AmountCents)
	require.NotEqual(t, uuid.Nil, p.PaymentID)
}

func TestReserved_RateOneFails(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	require.NoError(t, e.service(1).HandleInventoryReserved(context.Background(), reserved(t, oid, 2000)))

	status, _ := e.payment(t, oid)
	require.Equal(t, "FAILED", status)

	total, msg := e.published(t, events.PaymentFailed)
	require.EqualValues(t, 1, total)
	var p events.PaymentFailedPayload
	require.NoError(t, msg.Decode(&p))
	require.Equal(t, events.ReasonDeclined, p.Reason)
}

func TestReserved_DuplicateDeliveryChargesOnce(t *testing.T) {
	e := setup(t)
	env := reserved(t, uuid.New(), 2000)
	svc := e.service(0)
	require.NoError(t, svc.HandleInventoryReserved(context.Background(), env))
	require.NoError(t, svc.HandleInventoryReserved(context.Background(), env))

	require.Equal(t, 1, e.paymentCount(t))
	total, _ := e.published(t, events.PaymentSucceeded)
	require.EqualValues(t, 1, total)
}

func TestReserved_StoredOutcomeWinsOnRedelivery(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO payments (id, order_id, amount_cents, status, reason) VALUES ($1, $2, 2000, 'FAILED', 'declined')`,
		uuid.New(), oid)
	require.NoError(t, err)

	require.NoError(t, e.service(0).HandleInventoryReserved(context.Background(), reserved(t, oid, 2000)))

	status, _ := e.payment(t, oid)
	require.Equal(t, "FAILED", status)
	_, failed := e.published(t, events.PaymentFailed)
	require.Equal(t, oid, failed.OrderID)
	_, ok := e.published(t, events.PaymentSucceeded)
	require.Equal(t, uuid.Nil, ok.OrderID, "no payments.succeeded is published")
}

func TestReserved_LatencyHonoursContext(t *testing.T) {
	e := setup(t)
	svc := payments.NewService(e.pool, e.js, payments.Config{Latency: time.Minute}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := svc.HandleInventoryReserved(ctx, reserved(t, uuid.New(), 2000))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 0, e.paymentCount(t))
}
