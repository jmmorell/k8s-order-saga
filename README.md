# k8s-order-saga

Five event-driven Go services (orders, inventory, payments, notifications, load generator) built as a choreography over NATS JetStream.

A customer posts an order to `orders`. There is no orchestrator: each service reacts to the events of the others. `inventory` reserves stock, `payments` simulates a charge, and `orders` closes the order as `CONFIRMED` or `CANCELLED`. When a payment fails, `inventory` releases the stock it reserved (compensation). `notifications` emails the final result to Mailpit, and `loadgen` produces a steady stream of random orders.

The application is the workload for a Kubernetes portfolio: later days add kind, Envoy Gateway, observability, GitOps and a GKE deployment. Each service has its own database (one Postgres server, three logical databases) and ships as a small distroless image.

> The code in this repository was written by AI agents under the author's direction.

## The saga

```
POST /orders ─► orders: save PENDING ─► orders.created
orders.created      ─► inventory ─► inventory.reserved | inventory.rejected
inventory.reserved  ─► payments  ─► payments.succeeded | payments.failed
payments.failed     ─► inventory: release ─► inventory.released
inventory.rejected  ─► orders: CANCELLED(out_of_stock | unknown_sku) ─► orders.cancelled
payments.failed     ─► orders: CANCELLED(payment_failed)             ─► orders.cancelled
payments.succeeded  ─► orders: CONFIRMED                             ─► orders.confirmed
orders.confirmed / orders.cancelled ─► notifications
```

## Run it

Requires Docker with Compose.

```sh
make up      # creates .env with a random database password, builds the images, starts the stack
make smoke   # posts a confirmed and a cancelled order and checks the emails; prints SMOKE OK
make logs    # follow the logs of all services
make down    # stop the stack and delete its volumes
```

| What | Where |
|---|---|
| Orders API | `http://localhost:8080` (`POST /orders`, `GET /orders/{id}`) |
| Mailpit (emails) | `http://localhost:8025` |
| NATS monitoring | `http://localhost:8222` |

```sh
curl -X POST localhost:8080/orders -H 'Content-Type: application/json' \
  -d '{"customer_email":"me@example.local","items":[{"sku":"SKU-001","qty":2}]}'
```

Products are `SKU-001` to `SKU-009` (stock 1000, restocked every minute) and `SKU-010`, which is always out of stock.

Each service can be built on its own, which is what the Kubernetes days will use:

```sh
docker build --build-arg SERVICE=orders -t k8s-order-saga/orders:dev .
```

### Inject failures

```sh
# every payment is declined: orders end CANCELLED (payment_failed) and stock is released
PAYMENT_FAILURE_RATE=1 docker compose up -d payments
# back to the default (10 %)
docker compose up -d payments
```

`PAYMENT_LATENCY_MS`, `RATE_PER_SEC` and `OUT_OF_STOCK_RATIO` work the same way. `docker compose stop nats` makes `POST /orders` return 503 until NATS is back.

### Tests

```sh
make test    # unit, integration and end-to-end tests; the last two start containers
make lint
```

`tests/e2e` runs the four services against real Postgres, NATS and Mailpit containers and covers the happy path, out of stock, unknown SKU and payment failure with compensation.

## Decisions

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-monorepo-single-module.md) | Monorepo with a single Go module |
| [0002](docs/adr/0002-nats-jetstream-choreography.md) | NATS JetStream and choreography for the order saga |
| [0003](docs/adr/0003-at-least-once-idempotency-no-outbox.md) | At-least-once delivery, idempotent handlers, no outbox |
| [0004](docs/adr/0004-database-per-service.md) | One logical database per service |

## Delivery semantics

Events are delivered at least once. `notifications` has no database, so a
redelivered `orders.confirmed` or `orders.cancelled` event can send a
duplicate email.

## Known limitations

- **No outbox.** If `orders` crashes between committing an order and publishing `orders.created`, the order stays `PENDING` with no event. See ADR 0003.
- **A publish that times out can still have been accepted.** If `orders` deletes the order after a timeout on `orders.created` but the broker had already stored the event, the order is missing for the client while the saga continues: the result lands in `dlq.orders.*` and the stock stays reserved until the next restock. The outbox (iteration 2) removes this. See ADR 0003.
- **Duplicate emails are possible**, as described above.
- **Stopping a consumer does not wait for an in-flight handler.** A message being handled during shutdown is redelivered later, which idempotent handlers tolerate.

## Day 1 checkpoint

Before day 2 starts, the author checks that they own this part:

- [ ] Fresh clone, `make up`, `make smoke` → `SMOKE OK`
- [ ] Follow one order through the logs of the 4 services by `order_id`:
  `docker compose logs orders inventory payments notifications | grep <order_id>`
  (the id is in the `POST /orders` response; `make smoke` prints it). You should see, in order:
  1. `order created` (orders)
  2. `stock reserved` (inventory)
  3. `payment succeeded` (payments)
  4. `order confirmed` (orders)
  5. `email sent` (notifications)

  Redeliveries show up at debug level only, as `re-published stored outcome`.
- [ ] Explain in own words: choreography, at-least-once + idempotency, deterministic event IDs, DLQ, the missing outbox
- [ ] `docker compose stop nats`, POST an order → 503; start NATS → the next order completes
- [ ] Read the 4 ADRs and be able to defend them
