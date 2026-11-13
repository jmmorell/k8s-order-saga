package orders_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/orders"
	"k8s-portfolio/internal/platform/events"
	"k8s-portfolio/internal/platform/pgutil"
	"k8s-portfolio/internal/platform/testinfra"
)

type env struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	svc  *orders.Service
}

func setup(t *testing.T) env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, err := pgutil.Connect(ctx, testinfra.Postgres(t, "orders")["orders"])
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pgutil.Migrate(ctx, pool, orders.Migrations()))

	nc, js, err := events.Connect(testinfra.NATS(t), "test")
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	require.NoError(t, events.EnsureStreams(ctx, js))
	return env{pool: pool, js: js, svc: orders.NewService(pool, js, slog.New(slog.DiscardHandler))}
}

func (e env) orderCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&n))
	return n
}

func (e env) streamMsgs(t *testing.T, stream string) uint64 {
	t.Helper()
	ctx := context.Background()
	s, err := e.js.Stream(ctx, stream)
	require.NoError(t, err)
	info, err := s.Info(ctx)
	require.NoError(t, err)
	return info.State.Msgs
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body)))
	return rec
}

const validBody = `{"customer_email":"a@b.c","items":[{"sku":"SKU-001","qty":2}]}`

func TestCreate_InvalidInputReturns400AndStoresNothing(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{`,
		"unknown field":  `{"customer_email":"a@b.c","items":[{"sku":"SKU-001","qty":1}],"x":1}`,
		"no items":       `{"customer_email":"a@b.c","items":[]}`,
		"qty zero":       `{"customer_email":"a@b.c","items":[{"sku":"SKU-001","qty":0}]}`,
		"qty too big":    `{"customer_email":"a@b.c","items":[{"sku":"SKU-001","qty":101}]}`,
		"dup sku":        `{"customer_email":"a@b.c","items":[{"sku":"SKU-001","qty":1},{"sku":"SKU-001","qty":2}]}`,
		"bad email":      `{"customer_email":"nope","items":[{"sku":"SKU-001","qty":1}]}`,
		"body too large": `{"customer_email":"a@b.c","items":[{"sku":"` + strings.Repeat("x", 70_000) + `","qty":1}]}`,
	}
	e := setup(t)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := post(e.svc.Routes(), body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			var out map[string]string
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			require.NotEmpty(t, out["error"])
		})
	}
	require.Equal(t, 0, e.orderCount(t))
	require.EqualValues(t, 0, e.streamMsgs(t, "ORDERS"))
}

func TestValidate(t *testing.T) {
	ok := orders.CreateOrderRequest{CustomerEmail: "a@b.c", Items: []events.Item{{SKU: "S", Qty: 1}}}
	require.NoError(t, orders.Validate(ok))
	bad := ok
	bad.Items = []events.Item{{SKU: "", Qty: 1}}
	require.Error(t, orders.Validate(bad))
	bad.Items = make([]events.Item, 21)
	for i := range bad.Items {
		bad.Items[i] = events.Item{SKU: uuid.NewString(), Qty: 1}
	}
	require.Error(t, orders.Validate(bad))
}

func TestCreate_Valid202AndEventPublished(t *testing.T) {
	e := setup(t)
	rec := post(e.svc.Routes(), validBody)
	require.Equal(t, http.StatusAccepted, rec.Code)
	var out struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "PENDING", out.Status)

	s, err := e.js.Stream(context.Background(), "ORDERS")
	require.NoError(t, err)
	raw, err := s.GetLastMsgForSubject(context.Background(), string(events.OrderCreated))
	require.NoError(t, err)
	var env events.Envelope
	require.NoError(t, json.Unmarshal(raw.Data, &env))
	require.Equal(t, out.ID, env.OrderID)
	var p events.OrderCreatedPayload
	require.NoError(t, env.Decode(&p))
	require.Equal(t, "a@b.c", p.CustomerEmail)
	require.Equal(t, []events.Item{{SKU: "SKU-001", Qty: 2}}, p.Items)
}

func TestCreate_NATSDownReturns503AndRemovesOrder(t *testing.T) {
	e := setup(t)
	nc, js, err := events.Connect(testinfra.NATS(t), "closed")
	require.NoError(t, err)
	nc.Close()
	svc := orders.NewService(e.pool, js, slog.New(slog.DiscardHandler))

	rec := post(svc.Routes(), validBody)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.JSONEq(t, `{"error":"temporarily unavailable"}`, rec.Body.String())
	require.Equal(t, 0, e.orderCount(t))
}

func TestGet_UnknownIs404_BadIDIs400(t *testing.T) {
	e := setup(t)
	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.svc.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+id, nil))
		return rec
	}
	rec := get(uuid.NewString())
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, `{"error":"not found"}`, rec.Body.String())
	require.Equal(t, http.StatusBadRequest, get("nope").Code)
}

func TestGet_ReturnsStoredOrder(t *testing.T) {
	e := setup(t)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(post(e.svc.Routes(), validBody).Body.Bytes(), &created))

	rec := httptest.NewRecorder()
	e.svc.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/"+created.ID.String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var o orders.Order
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &o))
	require.Equal(t, created.ID, o.ID)
	require.Equal(t, orders.StatusPending, o.Status)
	require.Equal(t, "a@b.c", o.CustomerEmail)
	require.Equal(t, []events.Item{{SKU: "SKU-001", Qty: 2}}, o.Items)
	require.Nil(t, o.AmountCents)
}
