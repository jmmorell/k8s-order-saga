package inventory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/inventory"
	"github.com/jmmorell/k8s-order-saga/internal/platform/events"
	"github.com/jmmorell/k8s-order-saga/internal/platform/logx"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
	"github.com/jmmorell/k8s-order-saga/internal/platform/testinfra"
)

type env struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	svc  *inventory.Service
}

func setup(t *testing.T) env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, err := pgutil.Connect(ctx, testinfra.Postgres(t, "inventory")["inventory"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pgutil.Migrate(ctx, pool, inventory.Migrations()))

	nc, js, err := events.Connect(context.Background(), testinfra.NATS(t), "test")
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	require.NoError(t, events.EnsureStreams(ctx, js))

	return env{pool: pool, js: js, svc: inventory.NewService(pool, js, slog.New(slog.DiscardHandler))}
}

func orderCreated(t *testing.T, orderID uuid.UUID, items ...events.Item) events.Envelope {
	t.Helper()
	e, err := events.New(events.OrderCreated, orderID, events.OrderCreatedPayload{CustomerEmail: "a@b.c", Items: items})
	require.NoError(t, err)
	return e
}

func paymentFailed(t *testing.T, orderID uuid.UUID) events.Envelope {
	t.Helper()
	e, err := events.New(events.PaymentFailed, orderID, events.PaymentFailedPayload{Reason: events.ReasonDeclined})
	require.NoError(t, err)
	return e
}

func (e env) stock(t *testing.T, sku string) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE sku=$1`, sku).Scan(&n))
	return n
}

func (e env) reservation(t *testing.T, orderID uuid.UUID) (status, reason string) {
	t.Helper()
	var r *string
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT status, reason FROM reservations WHERE order_id=$1`, orderID).Scan(&status, &r))
	if r != nil {
		reason = *r
	}
	return status, reason
}

func (e env) reservationCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM reservations`).Scan(&n))
	return n
}

// published returns the number of messages in INVENTORY and the last envelope on subject.
func (e env) published(t *testing.T, subject events.Subject) (total uint64, last events.Envelope) {
	t.Helper()
	ctx := context.Background()
	s, err := e.js.Stream(ctx, "INVENTORY")
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

func TestOrderCreated_ReservesAndPublishesAmount(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, oid, events.Item{SKU: "SKU-001", Qty: 2}, events.Item{SKU: "SKU-002", Qty: 1})))

	require.Equal(t, 998, e.stock(t, "SKU-001"))
	require.Equal(t, 999, e.stock(t, "SKU-002"))
	status, _ := e.reservation(t, oid)
	require.Equal(t, "RESERVED", status)

	total, msg := e.published(t, events.InventoryReserved)
	require.EqualValues(t, 1, total)
	var p events.InventoryReservedPayload
	require.NoError(t, msg.Decode(&p))
	require.EqualValues(t, 2000, p.AmountCents)
	require.Equal(t, oid, msg.OrderID)
	require.Len(t, p.Items, 2)
}

func TestOrderCreated_OutOfStockRejects(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, oid, events.Item{SKU: "SKU-001", Qty: 1}, events.Item{SKU: "SKU-010", Qty: 1})))

	status, reason := e.reservation(t, oid)
	require.Equal(t, "REJECTED", status)
	require.Equal(t, events.ReasonOutOfStock, reason)
	require.Equal(t, 1000, e.stock(t, "SKU-001"), "no stock is touched on rejection")

	_, msg := e.published(t, events.InventoryRejected)
	var p events.InventoryRejectedPayload
	require.NoError(t, msg.Decode(&p))
	require.Equal(t, events.InventoryRejectedPayload{Reason: events.ReasonOutOfStock, SKU: "SKU-010"}, p)
}

func TestOrderCreated_UnknownSKURejects(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, oid, events.Item{SKU: "SKU-999", Qty: 1})))

	_, reason := e.reservation(t, oid)
	require.Equal(t, events.ReasonUnknownSKU, reason)
	_, msg := e.published(t, events.InventoryRejected)
	var p events.InventoryRejectedPayload
	require.NoError(t, msg.Decode(&p))
	require.Equal(t, events.InventoryRejectedPayload{Reason: events.ReasonUnknownSKU, SKU: "SKU-999"}, p)
}

func TestOrderCreated_InvalidQuantityIsPermanent(t *testing.T) {
	e := setup(t)
	err := e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, uuid.New(), events.Item{SKU: "SKU-001", Qty: -5}))
	require.ErrorIs(t, err, events.ErrPermanent)
	require.Equal(t, 1000, e.stock(t, "SKU-001"))
}

func TestOrderCreated_DuplicateDeliveryReservesOnce(t *testing.T) {
	e := setup(t)
	env := orderCreated(t, uuid.New(), events.Item{SKU: "SKU-001", Qty: 3})
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(), env))
	require.NoError(t, e.svc.HandleOrderCreated(context.Background(), env))

	require.Equal(t, 997, e.stock(t, "SKU-001"))
	require.Equal(t, 1, e.reservationCount(t))
	total, _ := e.published(t, events.InventoryReserved)
	require.EqualValues(t, 1, total)
}

func TestOrderCreated_StoredOutcomeIsPublishedOnRedelivery(t *testing.T) {
	e := setup(t)
	oid := uuid.New()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO reservations (order_id, items, status, amount_cents) VALUES ($1, '[{"sku":"SKU-001","qty":3}]', 'RESERVED', 1500)`, oid)
	require.NoError(t, err)

	require.NoError(t, e.svc.HandleOrderCreated(context.Background(),
		orderCreated(t, oid, events.Item{SKU: "SKU-001", Qty: 3})))

	require.Equal(t, 1000, e.stock(t, "SKU-001"), "stock is not taken twice")
	_, msg := e.published(t, events.InventoryReserved)
	var p events.InventoryReservedPayload
	require.NoError(t, msg.Decode(&p))
	require.EqualValues(t, 1500, p.AmountCents)
}

func TestOrderCreated_ConcurrentLastUnits(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, err := e.pool.Exec(ctx, `UPDATE products SET stock = 5 WHERE sku = 'SKU-001'`)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			errs <- e.svc.HandleOrderCreated(ctx, orderCreated(t, uuid.New(), events.Item{SKU: "SKU-001", Qty: 1}))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var reserved, rejected int
	require.NoError(t, e.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='RESERVED'), count(*) FILTER (WHERE status='REJECTED' AND reason='out_of_stock') FROM reservations`).
		Scan(&reserved, &rejected))
	require.Equal(t, 5, reserved)
	require.Equal(t, 5, rejected)
	require.Equal(t, 0, e.stock(t, "SKU-001"))
}

func TestPaymentFailed_ReleasesOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	oid := uuid.New()
	require.NoError(t, e.svc.HandleOrderCreated(ctx, orderCreated(t, oid, events.Item{SKU: "SKU-001", Qty: 4})))
	require.Equal(t, 996, e.stock(t, "SKU-001"))

	pf := paymentFailed(t, oid)
	require.NoError(t, e.svc.HandlePaymentFailed(ctx, pf))
	require.NoError(t, e.svc.HandlePaymentFailed(ctx, pf))

	require.Equal(t, 1000, e.stock(t, "SKU-001"))
	status, _ := e.reservation(t, oid)
	require.Equal(t, "RELEASED", status)
	_, msg := e.published(t, events.InventoryReleased)
	require.Equal(t, events.InventoryReleased, msg.Type)
	total, _ := e.published(t, events.InventoryReleased)
	require.EqualValues(t, 2, total, "one reserved plus one released")
}

func TestPaymentFailed_UnknownOrderIsPermanent(t *testing.T) {
	e := setup(t)
	err := e.svc.HandlePaymentFailed(context.Background(), paymentFailed(t, uuid.New()))
	require.ErrorIs(t, err, events.ErrPermanent)
}

func TestRestock_ResetsToRestockTo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	require.NoError(t, e.svc.HandleOrderCreated(ctx, orderCreated(t, uuid.New(), events.Item{SKU: "SKU-003", Qty: 7})))
	require.Equal(t, 993, e.stock(t, "SKU-003"))

	require.NoError(t, e.svc.Restock(ctx))
	require.Equal(t, 1000, e.stock(t, "SKU-003"))
	require.Equal(t, 0, e.stock(t, "SKU-010"))
}

func TestRun_StopsWhenContextEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	cfg := inventory.Config{
		HTTPAddr:        "127.0.0.1:0",
		DatabaseURL:     testinfra.Postgres(t, "inventory")["inventory"],
		NATSURL:         testinfra.NATS(t),
		RestockInterval: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()
	time.Sleep(3 * time.Second)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
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
	svc := inventory.NewService(e.pool, e.js, logx.NewWithWriter(&buf, "inventory", "debug"))
	oid := uuid.New()
	ctx := context.Background()
	in := orderCreated(t, oid, events.Item{SKU: "SKU-001", Qty: 2})

	require.NoError(t, svc.HandleOrderCreated(ctx, in))
	reserved := linesWith(logLines(t, &buf), "stock reserved")
	require.Len(t, reserved, 1)
	require.Equal(t, "INFO", reserved[0]["level"])
	require.Equal(t, oid.String(), reserved[0]["order_id"])
	require.EqualValues(t, 1000, reserved[0]["amount_cents"])

	require.NoError(t, svc.HandleOrderCreated(ctx, in))
	all := logLines(t, &buf)
	require.Len(t, linesWith(all, "stock reserved"), 1)
	re := linesWith(all, "re-published stored outcome")
	require.Len(t, re, 1)
	require.Equal(t, "DEBUG", re[0]["level"])
	require.Equal(t, oid.String(), re[0]["order_id"])
	require.Equal(t, string(events.InventoryReserved), re[0]["subject"])

	require.NoError(t, svc.HandlePaymentFailed(ctx, paymentFailed(t, oid)))
	released := linesWith(logLines(t, &buf), "stock released")
	require.Len(t, released, 1)
	require.Equal(t, "INFO", released[0]["level"])
	require.Equal(t, oid.String(), released[0]["order_id"])

	require.NoError(t, svc.HandlePaymentFailed(ctx, paymentFailed(t, oid)))
	all = logLines(t, &buf)
	require.Len(t, linesWith(all, "stock released"), 1)
	require.Len(t, linesWith(all, "re-published stored outcome"), 2)

	rej := uuid.New()
	bad := orderCreated(t, rej, events.Item{SKU: "NOPE", Qty: 1})
	require.NoError(t, svc.HandleOrderCreated(ctx, bad))
	rejected := linesWith(logLines(t, &buf), "stock rejected")
	require.Len(t, rejected, 1)
	require.Equal(t, "INFO", rejected[0]["level"])
	require.Equal(t, rej.String(), rejected[0]["order_id"])
	require.Equal(t, "NOPE", rejected[0]["sku"])
	require.NotEmpty(t, rejected[0]["reason"])
}
