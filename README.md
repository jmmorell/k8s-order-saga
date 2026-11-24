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
make order   # posts one SKU-001 order and prints it (ORDERS_URL overrides the target)
make dlq     # message count of every JetStream stream (the dlq stream shows dead letters)
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

## Run on Kubernetes (kind)

Day 2 runs the same services on a three-node [kind](https://kind.sigs.k8s.io/) cluster, deployed with one generic Helm chart and reached through Envoy Gateway. Requires Docker, `kind`, `kubectl`, `helm` and `openssl`.

```sh
make local-up      # creates the cluster order-saga and installs everything; safe to re-run
make local-smoke   # same checks as make smoke, against the cluster; prints SMOKE OK
make local-status  # nodes and all pods
make local-reload  # rebuilds the five images, loads them into kind and restarts the Deployments
make local-dlq     # message count of every JetStream stream
make local-down    # deletes the cluster, its data and its Secret
```

The orders API is at `http://localhost:8080/orders`. Only the `/orders` path is routed; `/healthz` and `/readyz` return 404 from outside. Every `kubectl` and `helm` call passes the context `kind-order-saga`, so the cluster never mixes with your other contexts. (`kind create cluster` does switch kubectl's current context to `kind-order-saga`; the scripts never rely on it.)

After changing code, use `make local-reload`: re-running `make local-up` rebuilds and loads the `:dev` images but does not restart running pods.

`make smoke` and `make local-smoke` retry the confirmed order up to 5 times when the simulated payment is declined.

Docker Compose and the kind cluster both use host port 8080, so run only one of them at a time. `make local-up` stops with a clear message if the port is taken (run `make down` first).

To understand every object, work through the guided tour: [docs/day2-tour.md](docs/day2-tour.md).

## Decisions

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-monorepo-single-module.md) | Monorepo with a single Go module |
| [0002](docs/adr/0002-nats-jetstream-choreography.md) | NATS JetStream and choreography for the order saga |
| [0003](docs/adr/0003-at-least-once-idempotency-no-outbox.md) | At-least-once delivery, idempotent handlers, no outbox |
| [0004](docs/adr/0004-database-per-service.md) | One logical database per service |
| [0005](docs/adr/0005-kind-local-cluster.md) | kind as the local Kubernetes cluster |
| [0006](docs/adr/0006-generic-helm-chart.md) | One generic Helm chart for the Go services |
| [0007](docs/adr/0007-gateway-api-envoy-gateway.md) | Gateway API with Envoy Gateway as the entry point |

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
  `ORDER_ID=...` (the id is in the `POST /orders` response; `make smoke` prints it), then
  `docker compose logs --no-log-prefix orders inventory payments notifications | grep "$ORDER_ID" | sort`.
  You should see, in order:
  1. `order created` (orders)
  2. `stock reserved` (inventory)
  3. `payment succeeded` (payments)
  4. `order confirmed` (orders)
  5. `email sent` (notifications)

  Every line starts with its timestamp, so `sort` makes the output chronological; lines a few milliseconds apart may swap, because `order created` is logged after the publish and a fast consumer can log first.

  Redeliveries show up at debug level only, as `re-published stored outcome`.
- [ ] Explain in own words: choreography, at-least-once + idempotency, deterministic event IDs, DLQ, the missing outbox
- [ ] `docker compose stop nats`, POST an order → 503; start NATS → the next order completes
- [ ] Read the 4 ADRs and be able to defend them

## Day 2 checkpoint

Before day 3 starts, the author checks that they own this part:

- [ ] `make local-down && make local-up && make local-smoke` → `SMOKE OK`
- [ ] Work through [docs/day2-tour.md](docs/day2-tour.md) top to bottom
- [ ] Explain in own words: Deployment vs StatefulSet, liveness vs readiness, Service vs Gateway/HTTPRoute, requests vs limits, why the Secret is created by a script
- [ ] Delete a pod of each service while watching `kubectl --context kind-order-saga get pods -n shop -w` (Postgres and NATS are in `-n platform`); confirm orders keep completing
- [ ] Read the ADRs 0005–0007 and be able to defend them
