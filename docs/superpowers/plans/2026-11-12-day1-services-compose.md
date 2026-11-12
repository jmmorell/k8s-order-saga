# Day 1 — Go Services + Docker Compose: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Five Go services running an order saga by pure choreography over NATS JetStream, with idempotent handlers and a dead-letter queue, working under `docker compose up` with a load generator.

**Architecture:** One Go module (`k8s-portfolio`) with `cmd/<service>` and `internal/<service>`, plus shared packages in `internal/platform`. Each service owns one Postgres database and talks to the others only through events. Handlers store their outcome under `order_id`, commit, then publish an event whose ID is derived from `(order_id, subject)`; redelivery re-publishes the stored outcome and JetStream deduplicates it. No outbox. One parameterised `Dockerfile` builds a distroless image per service.

**Tech Stack:** Go (latest stable, ≥1.25), `github.com/nats-io/nats.go` (+ `jetstream`), `github.com/jackc/pgx/v5`, `github.com/pressly/goose/v3`, `github.com/google/uuid`, `github.com/stretchr/testify`, `github.com/testcontainers/testcontainers-go`, `log/slog`, `net/http`, golangci-lint, Docker Compose, Postgres 17, NATS 2.11+, Mailpit.

**Spec:** `docs/superpowers/specs/2026-11-12-k8s-portfolio-design.md`

## Global Constraints

- Everything in the repo is in **English** (code, comments, commits, docs).
- Single Go module `k8s-portfolio` at the repo root.
- **Pure choreography**: no orchestrator.
- JetStream streams: `ORDERS` (`orders.*`), `INVENTORY` (`inventory.*`), `PAYMENTS` (`payments.*`), `DLQ` (`dlq.>`). File storage, `MaxAge` 7 days, duplicate window 2 min.
- Exact subjects: `orders.created`, `orders.confirmed`, `orders.cancelled`, `inventory.reserved`, `inventory.rejected`, `inventory.released`, `payments.succeeded`, `payments.failed`.
- Event envelope JSON: `event_id` (UUID), `type` (= subject), `occurred_at` (RFC3339 UTC), `order_id` (UUID), `payload` (object).
- `event_id` is **deterministic**: `uuid.NewSHA1(orderID, []byte(subject))`. Publish with header `Nats-Msg-Id` = `event_id`.
- Handlers: store outcome under `order_id` in a transaction → commit → publish. On redelivery, re-publish the stored outcome. **No outbox, no processed-events table.**
- Retries: `MaxDeliver` 5; Nak delays 1s, 5s, 15s, 30s; then publish to `dlq.<durable>.<original subject>` and `Term`.
- Order states: `PENDING → CONFIRMED | CANCELLED`; reasons `out_of_stock`, `unknown_sku`, `payment_failed`.
- One logical database per service (`orders`, `inventory`, `payments`); no cross-database reads.
- JSON logs to stdout via `slog`, attribute `service` on every line.
- Every service serves `/healthz` and `/readyz` on `HTTP_ADDR` (default `:8080`); graceful shutdown on SIGTERM/SIGINT (10 s).
- Env vars: `HTTP_ADDR`, `DATABASE_URL`, `NATS_URL`, `LOG_LEVEL`, `RESTOCK_INTERVAL`, `PAYMENT_FAILURE_RATE`, `PAYMENT_LATENCY_MS`, `SMTP_ADDR`, `MAIL_FROM`, `TARGET_URL`, `RATE_PER_SEC`, `OUT_OF_STOCK_RATIO`.
- Images: multi-stage, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`, final `gcr.io/distroless/static-debian12:nonroot`.
- **No secrets in the repo**: Compose reads `POSTGRES_PASSWORD` from a git-ignored `.env`; only `.env.example` is committed.
- Compose publishes to the host only: `orders` 8080, Mailpit UI 8025, NATS monitor 8222.
- A short ADR in `docs/adr/` precedes each platform piece. Between equivalent options, pick the easiest to explain.

## Review Focus

1. **Duplicate delivery** of an event → no double reservation or charge; the same outcome event is re-published with the same ID. Tests in Tasks 3 and 4.
2. **Concurrent orders for the last units of a SKU** → stock never negative; exactly as many succeed as units exist. Test in Task 3.
3. **Crash between commit and publish** in a handler (outcome stored, event never sent) → redelivery publishes the stored outcome without redoing the work. Tests in Tasks 3 and 4.
4. **Poison message** (handler always fails, or invalid JSON) → lands in the DLQ and does not block later messages. Test in Task 2.
5. **Bad or unlucky API calls**: invalid input → 400 with JSON error and nothing stored; NATS down → 503 and the order row is removed. Tests in Task 5.

---

## File Structure

```
go.mod  Makefile  .golangci.yml  .gitignore  .env.example  Dockerfile  docker-compose.yml  README.md
deploy/compose/postgres-init.sql
scripts/smoke.sh
docs/adr/0001-monorepo-single-module.md
docs/adr/0002-nats-jetstream-choreography.md
docs/adr/0003-at-least-once-idempotency-no-outbox.md
docs/adr/0004-database-per-service.md
cmd/{orders,inventory,payments,notifications,loadgen}/main.go
internal/platform/config/    env.go, env_test.go
internal/platform/logx/      logx.go
internal/platform/httpx/     health.go, serve.go, health_test.go
internal/platform/events/    envelope.go, payloads.go, streams.go, publish.go, consume.go, connect.go, *_test.go
internal/platform/pgutil/    pgutil.go, pgutil_test.go
internal/platform/testinfra/ containers.go
internal/inventory/   service.go, store.go, migrations/*.sql, *_test.go
internal/payments/    service.go, migrations/*.sql, *_test.go
internal/orders/      service.go, api.go, close.go, migrations/*.sql, *_test.go
internal/notifications/ service.go, sender.go, *_test.go
internal/loadgen/     service.go, generator.go, generator_test.go
tests/e2e/saga_test.go
```

Test convention: container-backed tests start with `if testing.Short() { t.Skip() }`. `make test-short` = unit only; `make test` = everything.

Every `cmd/<svc>/main.go` has the same shape: read env into the service `Config`, `logx.New("<svc>", LOG_LEVEL)`, `signal.NotifyContext(SIGINT, SIGTERM)`, call `<svc>.Run`, `os.Exit(1)` on error.

---

### Task 1: Repo skeleton, config, logging, health

**Files:**
- Create: `go.mod`, `Makefile`, `.golangci.yml`, `.gitignore`, `README.md` (stub), `docs/adr/0001-monorepo-single-module.md`
- Create: `internal/platform/config/env.go`, `internal/platform/logx/logx.go`, `internal/platform/httpx/{health.go,serve.go}`
- Test: `internal/platform/config/env_test.go`, `internal/platform/httpx/health_test.go`

**Interfaces:**
- Produces:
  - `config.String(key, def string) string`
  - `config.Required(key string) (string, error)`
  - `config.Int(key string, def int) (int, error)`, `config.Float(key string, def float64) (float64, error)`, `config.Duration(key string, def time.Duration) (time.Duration, error)` — parse errors name the variable
  - `logx.New(service, level string) *slog.Logger` — JSON to stdout, attr `service`; level debug|info|warn|error, default info
  - `type httpx.ReadyCheck func(ctx context.Context) error`
  - `httpx.HealthMux(checks map[string]httpx.ReadyCheck) *http.ServeMux` — `GET /healthz` → 200 `{"status":"ok"}`; `GET /readyz` → 200 `{"status":"ok"}` or 503 `{"status":"unavailable","failed":[…sorted names…]}`; 2 s timeout per check
  - `httpx.Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error` — `Shutdown` with 10 s timeout when `ctx` ends; nil on clean stop

- [ ] **Step 1:** `git init`; `go mod init k8s-portfolio`; `.gitignore` (`.env`, `bin/`, `coverage.out`); Makefile targets `test` (`go test -race ./...`), `test-short` (`go test -short -race ./...`), `lint` (`golangci-lint run`), `build` (`go build ./...`). Write ADR 0001.
- [ ] **Step 2: Failing tests**

```go
func TestInt_InvalidValueNamesVariable(t *testing.T) {
    t.Setenv("RATE", "abc")
    _, err := config.Int("RATE", 1)
    require.ErrorContains(t, err, "RATE")
}
func TestRequired_Empty(t *testing.T) { _, err := config.Required("MISSING_X"); require.Error(t, err) }

func TestReadyz_FailingChecksReturn503WithSortedNames(t *testing.T) {
    mux := httpx.HealthMux(map[string]httpx.ReadyCheck{
        "nats": func(context.Context) error { return errors.New("down") },
        "db":   func(context.Context) error { return errors.New("down") },
        "ok":   func(context.Context) error { return nil },
    })
    rec := httptest.NewRecorder()
    mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
    require.Equal(t, 503, rec.Code)
    require.JSONEq(t, `{"status":"unavailable","failed":["db","nats"]}`, rec.Body.String())
}
func TestHealthz_AlwaysOK(t *testing.T) { /* 200 {"status":"ok"} */ }
```

- [ ] **Step 3:** `go test ./internal/platform/...` → FAIL (does not compile).
- [ ] **Step 4:** Implement the signatures above.
- [ ] **Step 5:** `make test-short && make lint` → PASS.
- [ ] **Step 6:** Commit `chore: repo skeleton and platform packages (config, logging, health)`.

---

### Task 2: Events package (envelope, streams, publish, consume with DLQ)

**Files:**
- Create: `docs/adr/0002-nats-jetstream-choreography.md`
- Create: `internal/platform/events/{envelope.go,payloads.go,streams.go,publish.go,consume.go,connect.go}`, `internal/platform/testinfra/containers.go`
- Test: `internal/platform/events/envelope_test.go`, `internal/platform/events/consume_test.go`

**Interfaces:**
- Produces:
  - `type events.Subject string`; constants `OrderCreated, OrderConfirmed, OrderCancelled, InventoryReserved, InventoryRejected, InventoryReleased, PaymentSucceeded, PaymentFailed`
  - `type events.Envelope struct { ID uuid.UUID "event_id"; Type events.Subject "type"; OccurredAt time.Time "occurred_at"; OrderID uuid.UUID "order_id"; Payload json.RawMessage "payload" }`
  - `events.New(subject events.Subject, orderID uuid.UUID, payload any) (events.Envelope, error)` — `ID = uuid.NewSHA1(orderID, []byte(subject))`, `OccurredAt` UTC now
  - `(events.Envelope) Decode(v any) error` — failures wrap `events.ErrPermanent`
  - `var events.ErrPermanent`
  - Payloads (snake_case JSON): `Item{SKU string; Qty int}`; `OrderCreatedPayload{CustomerEmail string; Items []Item}`; `InventoryReservedPayload{Items []Item; AmountCents int64}`; `InventoryRejectedPayload{Reason, SKU string}`; `InventoryReleasedPayload{}`; `PaymentSucceededPayload{PaymentID uuid.UUID; AmountCents int64}`; `PaymentFailedPayload{Reason string}`; `OrderConfirmedPayload{CustomerEmail string; AmountCents int64}`; `OrderCancelledPayload{CustomerEmail, Reason string}`
  - Reason constants: `ReasonOutOfStock="out_of_stock"`, `ReasonUnknownSKU="unknown_sku"`, `ReasonPaymentFailed="payment_failed"`, `ReasonDeclined="declined"`
  - `events.Connect(url, name string) (*nats.Conn, jetstream.JetStream, error)` — `RetryOnFailedConnect(true)`, `MaxReconnects(-1)`
  - `events.EnsureStreams(ctx context.Context, js jetstream.JetStream) error` — idempotent `CreateOrUpdateStream` of the 4 streams
  - `events.Publish(ctx context.Context, js jetstream.JetStream, e events.Envelope) error`
  - `type events.Handler func(ctx context.Context, e events.Envelope) error`
  - `type events.ConsumerSpec struct { Stream, Durable string; Subjects []events.Subject; MaxDeliver int; NakDelays []time.Duration }` — defaults 5 and `[1s,5s,15s,30s]`; `AckWait` 30 s; explicit ack; deliver all
  - `events.Consume(ctx context.Context, js jetstream.JetStream, spec events.ConsumerSpec, h events.Handler, log *slog.Logger) (stop func(), err error)`
  - `testinfra.NATS(t *testing.T) (url string)`; `testinfra.Postgres(t *testing.T, dbs ...string) map[string]string` (db name → URL); `testinfra.Mailpit(t *testing.T) (smtpAddr, apiURL string)` — all with `t.Cleanup`

`Consume` algorithm (pinned by the tests):
- Invalid envelope JSON → DLQ + `Term`.
- Handler returns nil → `Ack`.
- `errors.Is(err, ErrPermanent)` → DLQ + `Term`.
- Other error: if `NumDelivered >= MaxDeliver` → DLQ + `Term`; else `NakWithDelay(NakDelays[min(NumDelivered-1, len-1)])`.
- DLQ publish: original bytes to `dlq.<Durable>.<original subject>` with headers `X-Error`, `X-Original-Subject`, `X-Delivery-Count`.

- [ ] **Step 1:** Write ADR 0002 (NATS JetStream vs RabbitMQ/Kafka; choreography vs orchestration).
- [ ] **Step 2: Failing tests**

```go
func TestNew_IDIsDeterministicPerOrderAndSubject(t *testing.T) {
    a, _ := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{})
    b, _ := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{})
    c, _ := events.New(events.OrderConfirmed, oid, events.OrderConfirmedPayload{})
    require.Equal(t, a.ID, b.ID); require.NotEqual(t, a.ID, c.ID)
}
func TestEnvelope_RoundTrip(t *testing.T) {
    e, _ := events.New(events.OrderCreated, oid, events.OrderCreatedPayload{CustomerEmail: "a@b.c", Items: []events.Item{{SKU: "SKU-001", Qty: 2}}})
    b, _ := json.Marshal(e)
    require.Contains(t, string(b), `"type":"orders.created"`)
    require.Contains(t, string(b), `"customer_email":"a@b.c"`)
    var back events.Envelope; require.NoError(t, json.Unmarshal(b, &back))
    var p events.OrderCreatedPayload; require.NoError(t, back.Decode(&p)); require.Equal(t, 2, p.Items[0].Qty)
}
func TestDecode_BadPayloadIsPermanent(t *testing.T) { /* Payload `"x"` into a struct → errors.Is(err, ErrPermanent) */ }

// integration (real NATS)
func TestPublish_SameEventTwiceIsStoredOnce(t *testing.T) { /* stream ORDERS has 1 message */ }
func TestConsume_AckOnSuccess(t *testing.T) { /* handler receives the envelope; NumAckPending ends at 0 */ }
func TestConsume_PoisonGoesToDLQAndDoesNotBlock(t *testing.T) {
    // spec: MaxDeliver 3, NakDelays [50ms]; handler always fails for order A, succeeds for order B (published after A)
    // within 10s: B handled; one message on "dlq.test.orders.created" with X-Delivery-Count "3", X-Original-Subject "orders.created"
}
func TestConsume_PermanentErrorGoesToDLQOnFirstDelivery(t *testing.T) { /* X-Delivery-Count "1" */ }
func TestConsume_InvalidJSONGoesToDLQ(t *testing.T) { /* raw "not-json" on orders.created → appears in DLQ */ }
```

- [ ] **Step 3:** `go test ./internal/platform/events/...` → FAIL.
- [ ] **Step 4:** Implement per Interfaces and the `Consume` algorithm.
- [ ] **Step 5:** `go test -race ./internal/platform/events/...` → PASS.
- [ ] **Step 6:** Commit `feat(events): envelope, JetStream streams, consume with retries and DLQ`.

---

### Task 3: Postgres helpers + `inventory` service

**Files:**
- Create: `docs/adr/0003-at-least-once-idempotency-no-outbox.md`, `docs/adr/0004-database-per-service.md`
- Create: `internal/platform/pgutil/pgutil.go`
- Create: `internal/inventory/{service.go,store.go}`, `internal/inventory/migrations/{00001_schema.sql,00002_seed.sql}`, `cmd/inventory/main.go`
- Test: `internal/platform/pgutil/pgutil_test.go`, `internal/inventory/store_test.go`, `internal/inventory/service_test.go`

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces:
  - `pgutil.Connect(ctx context.Context, url string) (*pgxpool.Pool, error)` — retries `Ping` every 1 s until success or `ctx` ends
  - `pgutil.Migrate(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error` — goose, embedded SQL
  - `pgutil.InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error`
  - `type inventory.Config struct { HTTPAddr, DatabaseURL, NATSURL string; RestockInterval time.Duration }`
  - `inventory.Run(ctx context.Context, cfg inventory.Config, log *slog.Logger) error` — connect, migrate, `EnsureStreams`, consumers, restock ticker, health server (checks `db`, `nats`)
  - `inventory.NewService(pool *pgxpool.Pool, js jetstream.JetStream, log *slog.Logger) *inventory.Service`
  - `(*inventory.Service) HandleOrderCreated(ctx context.Context, e events.Envelope) error`
  - `(*inventory.Service) HandlePaymentFailed(ctx context.Context, e events.Envelope) error`
  - `(*inventory.Service) Restock(ctx context.Context) error` — `UPDATE products SET stock = restock_to`
  - Consumers: stream `ORDERS`, durable `inventory`, `[orders.created]`; stream `PAYMENTS`, durable `inventory`, `[payments.failed]`

Schema:
- `products(sku text PK, name text NOT NULL, price_cents bigint NOT NULL, stock int NOT NULL CHECK (stock >= 0), restock_to int NOT NULL)`
- `reservations(order_id uuid PK, items jsonb NOT NULL, status text NOT NULL CHECK (status IN ('RESERVED','REJECTED','RELEASED')), reason text, sku text, amount_cents bigint, created_at timestamptz NOT NULL DEFAULT now())`

Seed: `SKU-001`…`SKU-009` with `price_cents = n*500`, `stock = restock_to = 1000`; `SKU-010` with `stock = restock_to = 0` (always out of stock).

`HandleOrderCreated`:
1. `InTx`: `SELECT … FROM reservations WHERE order_id=$1 FOR UPDATE`. If absent: lock product rows `FOR UPDATE` **in SKU order**; unknown SKU → insert `REJECTED/unknown_sku`; insufficient stock → insert `REJECTED/out_of_stock` (touch no stock); else decrement, insert `RESERVED` with `amount_cents = Σ price×qty`.
2. After commit, publish from the stored row: `RESERVED` → `inventory.reserved{items, amount_cents}`; `REJECTED` → `inventory.rejected{reason, sku}`; `RELEASED` → nothing.

`HandlePaymentFailed`:
1. `InTx`: row `FOR UPDATE`; absent → `ErrPermanent`; `RESERVED` → add stock back, set `RELEASED`.
2. After commit, if status is `RELEASED` → publish `inventory.released{}`.

- [ ] **Step 1:** Write ADR 0003 (why at-least-once + idempotent handlers + deterministic IDs; the commit/publish gap; outbox as the iteration-2 fix) and ADR 0004.
- [ ] **Step 2: Failing tests** (integration)

```go
func TestConnect_GivesUpWhenContextEnds(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second); defer cancel()
    start := time.Now(); _, err := pgutil.Connect(ctx, "postgres://u:p@127.0.0.1:1/x")
    require.Error(t, err); require.Less(t, time.Since(start), 4*time.Second)
}
func TestMigrate_TwiceIsNoop(t *testing.T) {}

func TestOrderCreated_ReservesAndPublishesAmount(t *testing.T) {
    // SKU-001 x2 + SKU-002 x1 → stock 998 / 999; reservation RESERVED amount 2000
    // stream INVENTORY has inventory.reserved with amount_cents 2000
}
func TestOrderCreated_OutOfStockRejects(t *testing.T) { /* SKU-010 → REJECTED out_of_stock, sku "SKU-010"; inventory.rejected published; no stock touched */ }
func TestOrderCreated_UnknownSKURejects(t *testing.T) { /* SKU-999 → reason "unknown_sku" */ }
func TestOrderCreated_DuplicateDeliveryReservesOnce(t *testing.T) {
    // same envelope handled twice → stock decremented once, 1 reservation row, stream INVENTORY has exactly 1 message
}
func TestOrderCreated_StoredOutcomeIsPublishedOnRedelivery(t *testing.T) {
    // simulate crash after commit: insert a RESERVED row (amount 1500) by hand, no event in the stream
    // handle the envelope → stock unchanged; inventory.reserved published with amount_cents 1500
}
func TestOrderCreated_ConcurrentLastUnits(t *testing.T) {
    // set SKU-001 stock = 5; 10 goroutines, distinct order IDs, qty 1
    // → exactly 5 RESERVED and 5 REJECTED/out_of_stock; final stock 0
}
func TestPaymentFailed_ReleasesOnce(t *testing.T) { /* handled twice → stock restored once; status RELEASED; 1 inventory.released in stream */ }
func TestPaymentFailed_UnknownOrderIsPermanent(t *testing.T) { /* errors.Is(err, events.ErrPermanent) */ }
func TestRestock_ResetsToRestockTo(t *testing.T) { /* after reservations → 1000; SKU-010 stays 0 */ }
```

- [ ] **Step 3:** `go test ./internal/platform/pgutil/... ./internal/inventory/...` → FAIL.
- [ ] **Step 4:** Implement. `main.go` env: `HTTP_ADDR` `:8080`, `DATABASE_URL` required, `NATS_URL` required, `RESTOCK_INTERVAL` `1m`, `LOG_LEVEL` `info`.
- [ ] **Step 5:** Same command with `-race` → PASS.
- [ ] **Step 6:** Commit `feat(inventory): reserve, reject, release and restock`.

---

### Task 4: `payments` service

**Files:**
- Create: `internal/payments/service.go`, `internal/payments/migrations/00001_schema.sql`, `cmd/payments/main.go`
- Test: `internal/payments/service_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces:
  - `type payments.Config struct { HTTPAddr, DatabaseURL, NATSURL string; FailureRate float64; Latency time.Duration }`
  - `(payments.Config) Validate() error` — `FailureRate` in [0,1], `Latency` ≥ 0
  - `payments.Run(ctx context.Context, cfg payments.Config, log *slog.Logger) error`
  - `payments.NewService(pool *pgxpool.Pool, js jetstream.JetStream, cfg payments.Config, log *slog.Logger) *payments.Service`
  - `(*payments.Service) HandleInventoryReserved(ctx context.Context, e events.Envelope) error`
  - Consumer: stream `INVENTORY`, durable `payments`, `[inventory.reserved]`

Schema: `payments(id uuid PK, order_id uuid UNIQUE NOT NULL, amount_cents bigint NOT NULL, status text NOT NULL CHECK (status IN ('SUCCEEDED','FAILED')), reason text, created_at timestamptz NOT NULL DEFAULT now())`.

Handler: wait `Latency` **outside the transaction** (honour `ctx`); decide `rand.Float64() < FailureRate`; `INSERT … ON CONFLICT (order_id) DO NOTHING`; read the row; after commit publish from the **stored** row: `SUCCEEDED` → `payments.succeeded{payment_id, amount_cents}`; `FAILED` → `payments.failed{reason:"declined"}`.

- [ ] **Step 1: Failing tests**

```go
func TestConfig_RejectsFailureRateOutOfRange(t *testing.T) { /* 1.5 and -0.1 → error; 0 and 1 → ok */ }
// integration
func TestReserved_RateZeroSucceeds(t *testing.T) { /* row SUCCEEDED amount 2000; payments.succeeded in stream */ }
func TestReserved_RateOneFails(t *testing.T) { /* row FAILED; payments.failed reason "declined" */ }
func TestReserved_DuplicateDeliveryChargesOnce(t *testing.T) { /* handled twice → 1 row, 1 message in stream PAYMENTS */ }
func TestReserved_StoredOutcomeWinsOnRedelivery(t *testing.T) {
    // insert a FAILED row by hand; service configured with FailureRate 0
    // handle → still FAILED; payments.failed published (not succeeded)
}
```

- [ ] **Step 2:** `go test ./internal/payments/...` → FAIL.
- [ ] **Step 3:** Implement. `main.go`: `PAYMENT_FAILURE_RATE` default `0.1`, `PAYMENT_LATENCY_MS` default `200`; exit with error if `Validate` fails.
- [ ] **Step 4:** `go test -race ./internal/payments/...` → PASS.
- [ ] **Step 5:** Commit `feat(payments): simulated charge with configurable failure rate and latency`.

---

### Task 5: `orders` service (API + saga closing)

**Files:**
- Create: `internal/orders/{service.go,api.go,close.go}`, `internal/orders/migrations/00001_schema.sql`, `cmd/orders/main.go`
- Test: `internal/orders/api_test.go`, `internal/orders/close_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces:
  - `type orders.CreateOrderRequest struct { CustomerEmail string "customer_email"; Items []events.Item "items" }`
  - `orders.Validate(r orders.CreateOrderRequest) error` (exported; `loadgen` tests use it)
  - `type orders.Status string`: `StatusPending="PENDING"`, `StatusConfirmed="CONFIRMED"`, `StatusCancelled="CANCELLED"`
  - `type orders.Order struct { ID uuid.UUID "id"; Status orders.Status "status"; Reason string "reason,omitempty"; CustomerEmail string "customer_email"; Items []events.Item "items"; AmountCents *int64 "amount_cents,omitempty"; CreatedAt time.Time "created_at"; UpdatedAt time.Time "updated_at" }`
  - `type orders.Config struct { HTTPAddr, DatabaseURL, NATSURL string }`
  - `orders.Run(ctx context.Context, cfg orders.Config, log *slog.Logger) error`
  - `orders.NewService(pool *pgxpool.Pool, js jetstream.JetStream, log *slog.Logger) *orders.Service`
  - `(*orders.Service) Routes() http.Handler` — API routes (mounted next to the health mux)
  - `(*orders.Service) HandleResult(ctx context.Context, e events.Envelope) error` — handles `inventory.rejected`, `payments.succeeded`, `payments.failed`
  - HTTP: `POST /orders` → 202 `{"id":"…","status":"PENDING"}` | 400 `{"error":"…"}` | 503 `{"error":"temporarily unavailable"}`; `GET /orders/{id}` → 200 `Order` | 400 (not a UUID) | 404 `{"error":"not found"}`
  - Consumers: stream `INVENTORY`, durable `orders`, `[inventory.rejected]`; stream `PAYMENTS`, durable `orders`, `[payments.succeeded, payments.failed]`

`Validate` rules: email non-empty and contains `@`; 1–20 items; `qty` 1–100; SKU non-empty; **no duplicate SKUs**. Decoder uses `DisallowUnknownFields` and `http.MaxBytesReader` of 64 KiB.

Schema: `orders(id uuid PK, customer_email text NOT NULL, items jsonb NOT NULL, status text NOT NULL, reason text, amount_cents bigint, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`.

Create: insert `PENDING` → publish `orders.created`; if publish fails, **delete the row** and return 503.

`HandleResult`: `InTx`: row `FOR UPDATE`; absent → `ErrPermanent`; if `PENDING`: `inventory.rejected` → `CANCELLED` with the payload's reason; `payments.failed` → `CANCELLED/payment_failed`; `payments.succeeded` → `CONFIRMED` + `amount_cents`. After commit publish from the **stored** state: `CONFIRMED` → `orders.confirmed{customer_email, amount_cents}`; `CANCELLED` → `orders.cancelled{customer_email, reason}`.

- [ ] **Step 1: Failing tests**

```go
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
    // each: 400, Content-Type application/json, body has "error"; afterwards 0 rows in orders and 0 messages in stream ORDERS
}
func TestCreate_Valid202AndEventPublished(t *testing.T) { /* 202 PENDING; orders.created in stream with the same order_id */ }
func TestCreate_NATSDownReturns503AndRemovesOrder(t *testing.T) { /* service built with a closed NATS connection → 503; 0 rows */ }
func TestGet_UnknownIs404_BadIDIs400(t *testing.T) {}
func TestResult_PaymentSucceededConfirms(t *testing.T) { /* CONFIRMED, amount_cents 2000, orders.confirmed published */ }
func TestResult_InventoryRejectedCancelsWithReason(t *testing.T) { /* reason "out_of_stock" */ }
func TestResult_PaymentFailedCancels(t *testing.T) { /* reason "payment_failed" */ }
func TestResult_DuplicateDeliveryPublishesOnce(t *testing.T) { /* handled twice → 1 orders.confirmed in stream */ }
func TestResult_UnknownOrderIsPermanent(t *testing.T) { /* errors.Is(err, events.ErrPermanent) */ }
```

- [ ] **Step 2:** `go test ./internal/orders/...` → FAIL.
- [ ] **Step 3:** Implement per Interfaces.
- [ ] **Step 4:** `go test -race ./internal/orders/...` → PASS.
- [ ] **Step 5:** Commit `feat(orders): order API and saga closing`.

---

### Task 6: `notifications` and `loadgen`

**Files:**
- Create: `internal/notifications/{service.go,sender.go}`, `cmd/notifications/main.go`
- Create: `internal/loadgen/{service.go,generator.go}`, `cmd/loadgen/main.go`
- Test: `internal/notifications/service_test.go`, `internal/loadgen/generator_test.go`

**Interfaces:**
- Consumes: Tasks 1–2; `orders.CreateOrderRequest`, `orders.Validate`.
- Produces:
  - `type notifications.Sender interface { Send(ctx context.Context, to, subject, body string) error }`
  - `notifications.NewSMTPSender(addr, from string) notifications.Sender` (`net/smtp`, no auth)
  - `notifications.NewService(s notifications.Sender, log *slog.Logger) *notifications.Service`; `(*notifications.Service) Handle(ctx context.Context, e events.Envelope) error`
  - `notifications.Run(ctx context.Context, cfg notifications.Config, log *slog.Logger) error` with `Config{HTTPAddr, NATSURL, SMTPAddr, From string}`; readiness check `nats` only
  - Consumer: stream `ORDERS`, durable `notifications`, `[orders.confirmed, orders.cancelled]`
  - Exact mail subjects: `Order <id> confirmed` / `Order <id> cancelled (<reason>)`
  - `loadgen.RandomOrder(r *rand.Rand, outOfStockRatio float64, n int) orders.CreateOrderRequest` — with probability `outOfStockRatio` a single item `SKU-010`; otherwise 1–3 **distinct** SKUs from `SKU-001`…`SKU-009`, qty 1–3; email `loadgen+<n>@example.local`
  - `loadgen.Run(ctx context.Context, cfg loadgen.Config, log *slog.Logger) error` with `Config{HTTPAddr, TargetURL string; Rate, OutOfStockRatio float64}` — `POST <TargetURL>/orders` at `Rate`/s, 5 s timeout per request, logs status-code counts every 30 s

`notifications` has no database: delivery is at-least-once, so a redelivery can send a duplicate email. State this in the README.

- [ ] **Step 1: Failing tests**

```go
func TestHandle_ConfirmedSendsEmail(t *testing.T) { /* fake Sender: to "a@b.c", subject "Order <id> confirmed" */ }
func TestHandle_CancelledIncludesReason(t *testing.T) { /* subject "Order <id> cancelled (out_of_stock)" */ }
func TestHandle_SenderErrorIsRetryable(t *testing.T) { /* error is NOT ErrPermanent */ }
func TestSMTPSender_DeliversToMailpit(t *testing.T) { /* integration: Mailpit GET /api/v1/messages shows 1 message with that Subject */ }

func TestRandomOrder_AlwaysValid(t *testing.T) { /* 1000 orders, fixed seed → orders.Validate(o) == nil for all */ }
func TestRandomOrder_RatioOneIsAlwaysSKU010(t *testing.T) {}
func TestRandomOrder_RatioZeroNeverSKU010(t *testing.T) {}
```

- [ ] **Step 2:** `go test ./internal/notifications/... ./internal/loadgen/...` → FAIL.
- [ ] **Step 3:** Implement. Env: `SMTP_ADDR` required, `MAIL_FROM` default `shop@example.local`; `TARGET_URL` required, `RATE_PER_SEC` default `2`, `OUT_OF_STOCK_RATIO` default `0.05`.
- [ ] **Step 4:** Same command with `-race` → PASS.
- [ ] **Step 5:** Commit `feat: notifications service and load generator`.

---

### Task 7: End-to-end test, Docker, Compose, README

**Files:**
- Create: `tests/e2e/saga_test.go`, `Dockerfile`, `docker-compose.yml`, `.env.example`, `deploy/compose/postgres-init.sql`, `scripts/smoke.sh`
- Modify: `Makefile` (add `up`, `down`, `logs`, `smoke`), `README.md`

**Interfaces:**
- Consumes: every `Run` function, `testinfra.*`.
- Produces:
  - `docker build --build-arg SERVICE=<svc> -t k8s-portfolio/<svc>:dev .` for each service (day 2 depends on this)
  - `make up` — creates `.env` from `.env.example` with a random `POSTGRES_PASSWORD` (`openssl rand -hex 16`) if missing, then `docker compose up --build -d`
  - `make smoke` — runs `scripts/smoke.sh`, prints `SMOKE OK` and exits 0 on success

E2E helper: `startStack(t *testing.T, failureRate float64) (ordersURL, mailpitAPI string, inventoryDB *pgxpool.Pool)` — Postgres (dbs `orders`, `inventory`, `payments`), NATS, Mailpit containers; the 4 services in goroutines on free ports, `RestockInterval` 1 h, `Latency` 0; waits for every `/readyz` to return 200.

Dockerfile: `build` stage on the Go image matching `go.mod`, `go mod download` before copying sources, `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${SERVICE}`; final stage `gcr.io/distroless/static-debian12:nonroot`, `USER nonroot:nonroot`, `ENTRYPOINT ["/app"]`.

Compose: `postgres:17-alpine` (init SQL creates the 3 databases; healthcheck `pg_isready`); `nats:2.11-alpine` with `-js -m 8222` (healthcheck on `:8222/healthz`); `axllent/mailpit`; the 5 services with `depends_on: {condition: service_healthy}`, `read_only: true`, `cap_drop: [ALL]`, `security_opt: ["no-new-privileges:true"]`.

- [ ] **Step 1: E2E tests**

```go
func TestSaga_HappyPath(t *testing.T) {
    // failureRate 0; POST {"customer_email":"happy@example.local","items":[{"sku":"SKU-001","qty":2}]}
    // within 15s: GET → CONFIRMED, amount_cents 1000; stock SKU-001 == 998
    // Mailpit has a message to happy@example.local with subject "Order <id> confirmed"
}
func TestSaga_OutOfStock(t *testing.T) { /* SKU-010 → CANCELLED "out_of_stock"; mail "cancelled (out_of_stock)" */ }
func TestSaga_UnknownSKU(t *testing.T) { /* SKU-999 → CANCELLED "unknown_sku" */ }
func TestSaga_PaymentFailureCompensates(t *testing.T) {
    // failureRate 1; SKU-002 qty 3 → CANCELLED "payment_failed"
    // eventually stock SKU-002 == 1000 and reservations.status == "RELEASED"
}
```

- [ ] **Step 2:** `go test -race ./tests/e2e/...` → PASS. A failure here is a bug in Tasks 3–6: fix it there, with a unit test that reproduces it.
- [ ] **Step 3:** Write `scripts/smoke.sh`: POST an order for `SKU-001` to `localhost:8080`, wait ≤ 20 s for `CONFIRMED`; POST `SKU-010`, wait for `CANCELLED`; check `localhost:8025/api/v1/messages` has ≥ 2 messages. Run it → fails (no stack yet).
- [ ] **Step 4:** Write Dockerfile, Compose file, `.env.example`, init SQL, Makefile targets.
- [ ] **Step 5:** `make up && make smoke` → `SMOKE OK`. `docker images k8s-portfolio/orders:dev` → under 30 MB. `git status` does not list `.env`.
- [ ] **Step 6:** `PAYMENT_FAILURE_RATE=1 docker compose up -d payments`, wait 30 s → Mailpit shows `cancelled (payment_failed)` mails. Restore with `docker compose up -d payments`.
- [ ] **Step 7:** README: what the project is (two paragraphs), the saga diagram from the spec §5, how to run (`make up`, `make smoke`, Mailpit at `localhost:8025`, NATS monitor at `localhost:8222`), how to inject failures, ADR index, known limitations (no outbox; duplicate emails), and a line stating the code was written by AI agents under the author's direction.
- [ ] **Step 8:** Add a **"Day 1 checkpoint"** section for the author:
  - [ ] Fresh clone, `make up`, `make smoke` → OK
  - [ ] Follow one order through the logs of the 4 services by `order_id`
  - [ ] Explain in own words: choreography, at-least-once + idempotency, deterministic event IDs, DLQ, the missing outbox
  - [ ] `docker compose stop nats`, POST an order → 503; start NATS → next order completes
  - [ ] Read the 4 ADRs and be able to defend them
- [ ] **Step 9:** `make lint && make test` → PASS. Commit `build: e2e saga test, distroless images, Compose stack and README`.
- [ ] **Step 10:** **Stop.** Day 2 starts only after the author confirms the checkpoint.

---

## Next days (each gets its own guide or plan when it starts)

Days 2, 4, 5 and 6 are **guided sessions where the author types**; days 3 and the CI part of day 4 are agent plans. They are written the day before, because each day's result changes the next day's details.

| Day | Deliverable | Depends on |
|---|---|---|
| 2 | `make local-up`: kind (1+2 nodes), Envoy Gateway, NATS, Postgres StatefulSet, generic `charts/service`; `curl` to `orders` through the Gateway works | Task 7 images |
| 3 | OTel in services (HTTP, pgx, NATS with `traceparent` headers), Collector, Prometheus/Tempo/(Loki)/Grafana, saga dashboard, SLO, 3 alerts | Day 2 |
| 4 | Default-deny NetworkPolicies, PSS `restricted`, per-service ServiceAccounts; Argo CD app-of-apps; `workflow_dispatch`: test → multi-arch build → Trivy → GHCR → tag-bump commit | Days 2–3 |
| 5 | Terraform: VPC, GKE 3×`e2-standard-2` spot, Cloud SQL + Auth Proxy with Workload Identity; `make gcp-up`, capture evidence, `make gcp-down` | Day 4 |
| 6 | Postmortems for a payment outage and a bad rollout; KEDA on NATS lag if time allows | Days 3–4 |
| 7 | README, architecture diagram, cost table, decisions table, iteration-2 roadmap | All |
