# Backlog — deferred findings from day-1 reviews

Minor findings the task and final reviews raised and deliberately left open. None blocks day 2.

## Messaging (`internal/platform/events`)
- Cancelling the context passed to `Consume` does not stop the subscription; callers must call `stop()`.
- `stop()` does not wait for an in-flight handler (documented in the README).
- The default pull buffer can exceed `AckWait` when handlers are slow, burning delivery attempts.
- An empty `Subjects` list consumes the whole stream; it should be a validation error.
- DLQ publishes carry no `Nats-Msg-Id`, so a failed `Term` after a successful DLQ publish duplicates the DLQ entry.
- `Connect` takes no context: a SIGTERM during its 30 s start-up wait is not honoured.
- Test tidiness in `consume_test.go`.
- Saga logging: if a commit succeeds but the publish fails, the redelivery only logs DEBUG `re-published stored outcome`, so that transition never gets an INFO line.

## Health (`internal/platform/httpx`)
- `/readyz` discards the failing check's error (not logged, not in the body).
- No test for the 2 s per-check timeout or for `Serve` on an address in use.

## inventory
- `Restock` locks rows in heap order while handlers lock in SKU order: a rare, self-healing deadlock is possible.
- `TestRun_StopsWhenContextEnds` uses a fixed 3 s sleep and does not prove `stop()` ran.

## payments
- The simulated latency runs before checking for an existing payment, so redeliveries sleep needlessly.
- A test helper counts all messages in the PAYMENTS stream instead of per subject.

## orders
- `inventory.rejected` with an empty reason cancels the order with a NULL reason (no producer emits it today).
- Unknown routes and 405s return plain text instead of the JSON error shape.
- The late-result test does not assert the re-published event; `Run` has no automated test.

## notifications / loadgen
- SMTP 5xx replies are retried like transient errors (about 50 s before the DLQ).
- A failed `QUIT` after the message was accepted causes a redelivery and a duplicate email.
- The `To` header uses the raw recipient string; non-ASCII subjects are not RFC 2047 encoded; `headerSafe` is untested.
- `Send` honours the context only while dialling.
- loadgen: a rate above 1e9 panics the ticker; the last partial tally is not logged on shutdown.

## Compose / tests
- Postgres, NATS and Mailpit containers do not carry the hardening flags the Go services have.
- `make up` keeps a hand-copied `.env` with the placeholder password.
- The e2e `stock()` helper calls `require` inside an `Eventually` goroutine.

## Design questions noted by the final review (not defects)
- Restock overwrites stock regardless of open reservations, so a later release can push stock above `restock_to`.
- One Postgres superuser serves the three databases; isolation is by convention, not by grants.
- `customer_email` is validated only by "contains @".
- `Consume` does not reject an envelope whose `type` differs from its subject or whose `order_id` is nil.
- Payments are handled one at a time per consumer, so raising `PAYMENT_LATENCY_MS` builds a backlog.
