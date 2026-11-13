package orders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/orders"
	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
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

// logLines parses the JSON lines written by a logx logger.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(l, &m))
		out = append(out, m)
	}
	return out
}

// linesWith returns the lines with the given msg.
func linesWith(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

func TestLogs_TransitionsAtInfoRedeliveryAtDebug(t *testing.T) {
	e := setup(t)
	var buf bytes.Buffer
	svc := orders.NewService(e.pool, e.js, logx.NewWithWriter(&buf, "orders", "debug"))

	rec := post(svc.Routes(), validBody)
	require.Equal(t, http.StatusAccepted, rec.Code)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	id := created.ID.String()

	lines := linesWith(logLines(t, &buf), "order created")
	require.Len(t, lines, 1)
	require.Equal(t, "INFO", lines[0]["level"])
	require.Equal(t, id, lines[0]["order_id"])
	require.EqualValues(t, 1, lines[0]["items"])

	env := mustEnv(t, events.PaymentSucceeded, created.ID, events.PaymentSucceededPayload{PaymentID: uuid.New(), AmountCents: 2000})
	require.NoError(t, svc.HandleResult(context.Background(), env))
	all := logLines(t, &buf)
	confirmed := linesWith(all, "order confirmed")
	require.Len(t, confirmed, 1)
	require.Equal(t, "INFO", confirmed[0]["level"])
	require.Equal(t, id, confirmed[0]["order_id"])
	require.EqualValues(t, 2000, confirmed[0]["amount_cents"])
	require.Empty(t, linesWith(all, "re-published stored outcome"))

	require.NoError(t, svc.HandleResult(context.Background(), env))
	all = logLines(t, &buf)
	require.Len(t, linesWith(all, "order confirmed"), 1)
	re := linesWith(all, "re-published stored outcome")
	require.Len(t, re, 1)
	require.Equal(t, "DEBUG", re[0]["level"])
	require.Equal(t, id, re[0]["order_id"])
	require.Equal(t, string(events.OrderConfirmed), re[0]["subject"])
}
