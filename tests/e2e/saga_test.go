// Package e2e runs the four services of the saga against real Postgres,
// NATS and Mailpit containers.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jmmorell/k8s-order-saga/internal/inventory"
	"github.com/jmmorell/k8s-order-saga/internal/notifications"
	"github.com/jmmorell/k8s-order-saga/internal/orders"
	"github.com/jmmorell/k8s-order-saga/internal/payments"
	"github.com/jmmorell/k8s-order-saga/internal/platform/pgutil"
	"github.com/jmmorell/k8s-order-saga/internal/platform/testinfra"
)

const (
	finalTimeout = 15 * time.Second
	pollEvery    = 100 * time.Millisecond
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// startStack runs the four services in goroutines and waits until all are ready.
func startStack(t *testing.T, failureRate float64) (ordersURL, mailpitAPI string, inventoryDB *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	dbs := testinfra.Postgres(t, "orders", "inventory", "payments")
	natsURL := testinfra.NATS(t)
	smtpAddr, mailpitAPI := testinfra.Mailpit(t)

	ordersAddr, inventoryAddr, paymentsAddr, notificationsAddr :=
		freeAddr(t), freeAddr(t), freeAddr(t), freeAddr(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	runs := map[string]func() error{
		"orders": func() error {
			return orders.Run(ctx, orders.Config{HTTPAddr: ordersAddr, DatabaseURL: dbs["orders"], NATSURL: natsURL}, log)
		},
		"inventory": func() error {
			return inventory.Run(ctx, inventory.Config{HTTPAddr: inventoryAddr, DatabaseURL: dbs["inventory"],
				NATSURL: natsURL, RestockInterval: time.Hour}, log)
		},
		"payments": func() error {
			return payments.Run(ctx, payments.Config{HTTPAddr: paymentsAddr, DatabaseURL: dbs["payments"],
				NATSURL: natsURL, FailureRate: failureRate, Latency: 0}, log)
		},
		"notifications": func() error {
			return notifications.Run(ctx, notifications.Config{HTTPAddr: notificationsAddr, NATSURL: natsURL,
				SMTPAddr: smtpAddr, From: "shop@example.local"}, log)
		},
	}
	done := make(chan string, len(runs))
	errs := make(chan error, len(runs))
	for name, run := range runs {
		go func() {
			if err := run(); err != nil {
				errs <- fmt.Errorf("%s: %w", name, err)
			}
			done <- name
		}()
	}
	t.Cleanup(func() {
		cancel()
		for range runs {
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Error("a service did not stop after cancel")
				return
			}
		}
		select {
		case err := <-errs:
			t.Errorf("service stopped with error: %v", err)
		default:
		}
	})

	for _, addr := range []string{ordersAddr, inventoryAddr, paymentsAddr, notificationsAddr} {
		url := "http://" + addr + "/readyz"
		require.Eventually(t, func() bool {
			select {
			case err := <-errs:
				t.Errorf("service failed to start: %v", err) // not Fatalf: this runs outside the test goroutine
				return true
			default:
			}
			resp, err := http.Get(url)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode == http.StatusOK
		}, 30*time.Second, pollEvery, "%s never became ready", url)
	}

	pool, err := pgutil.Connect(context.Background(), dbs["inventory"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return "http://" + ordersAddr, mailpitAPI, pool
}

type orderView struct {
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	AmountCents int64  `json:"amount_cents"`
}

func postOrder(t *testing.T, ordersURL, email, sku string, qty int) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"customer_email": email,
		"items":          []map[string]any{{"sku": sku, "qty": qty}},
	})
	require.NoError(t, err)
	resp, err := http.Post(ordersURL+"/orders", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.ID
}

// awaitFinal waits until the order leaves PENDING and returns its final view.
func awaitFinal(t *testing.T, ordersURL string, id uuid.UUID) orderView {
	t.Helper()
	var v orderView
	require.Eventually(t, func() bool {
		resp, err := http.Get(ordersURL + "/orders/" + id.String())
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		v = orderView{}
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&v) == nil && v.Status != "PENDING"
	}, finalTimeout, pollEvery, "order %s never left PENDING", id)
	return v
}

func stock(t *testing.T, pool *pgxpool.Pool, sku string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE sku = $1`, sku).Scan(&n))
	return n
}

func requireMail(t *testing.T, mailpitAPI, to, subject string) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := http.Get(mailpitAPI + "/api/v1/messages")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Messages []struct {
				Subject string
				To      []struct{ Address string }
			}
		}
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			return false
		}
		for _, m := range out.Messages {
			for _, r := range m.To {
				if m.Subject == subject && strings.EqualFold(r.Address, to) {
					return true
				}
			}
		}
		return false
	}, finalTimeout, pollEvery, "no mail to %s with subject %q", to, subject)
}

func TestSaga_HappyPath(t *testing.T) {
	ordersURL, mailpit, invDB := startStack(t, 0)
	id := postOrder(t, ordersURL, "happy@example.local", "SKU-001", 2)

	v := awaitFinal(t, ordersURL, id)
	require.Equal(t, "CONFIRMED", v.Status)
	require.EqualValues(t, 1000, v.AmountCents)
	require.Equal(t, 998, stock(t, invDB, "SKU-001"))
	requireMail(t, mailpit, "happy@example.local", fmt.Sprintf("Order %s confirmed", id))
}

func TestSaga_OutOfStock(t *testing.T) {
	ordersURL, mailpit, _ := startStack(t, 0)
	id := postOrder(t, ordersURL, "oos@example.local", "SKU-010", 1)

	v := awaitFinal(t, ordersURL, id)
	require.Equal(t, "CANCELLED", v.Status)
	require.Equal(t, "out_of_stock", v.Reason)
	requireMail(t, mailpit, "oos@example.local", fmt.Sprintf("Order %s cancelled (out_of_stock)", id))
}

func TestSaga_UnknownSKU(t *testing.T) {
	ordersURL, mailpit, _ := startStack(t, 0)
	id := postOrder(t, ordersURL, "unknown@example.local", "SKU-999", 1)

	v := awaitFinal(t, ordersURL, id)
	require.Equal(t, "CANCELLED", v.Status)
	require.Equal(t, "unknown_sku", v.Reason)
	requireMail(t, mailpit, "unknown@example.local", fmt.Sprintf("Order %s cancelled (unknown_sku)", id))
}

func TestSaga_PaymentFailureCompensates(t *testing.T) {
	ordersURL, _, invDB := startStack(t, 1)
	id := postOrder(t, ordersURL, "declined@example.local", "SKU-002", 3)

	v := awaitFinal(t, ordersURL, id)
	require.Equal(t, "CANCELLED", v.Status)
	require.Equal(t, "payment_failed", v.Reason)

	require.Eventually(t, func() bool {
		var status string
		err := invDB.QueryRow(context.Background(),
			`SELECT status FROM reservations WHERE order_id = $1`, id).Scan(&status)
		return err == nil && status == "RELEASED" && stock(t, invDB, "SKU-002") == 1000
	}, finalTimeout, pollEvery, "stock for %s was never released", id)
}
