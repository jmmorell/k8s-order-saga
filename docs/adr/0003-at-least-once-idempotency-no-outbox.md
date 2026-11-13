# ADR 0003: At-least-once delivery, idempotent handlers, no outbox

## Status

Accepted

## Context

A handler changes its own database and then tells the other services by publishing an event. The database and the broker cannot share a transaction, so a crash can fall between the commit and the publish. We must choose which failure to accept: a lost event or a duplicated one.

## Decision

- Delivery is at least once. A message is acknowledged only after the handler returns nil; otherwise it is redelivered.
- Every handler stores its outcome under the `order_id` in one transaction, commits, and only then publishes the event built from the stored row.
- On redelivery the handler finds the stored outcome and publishes it again instead of repeating the work.
- Event IDs are deterministic (`uuid.NewSHA1(orderID, subject)`) and sent as `Nats-Msg-Id`, so JetStream drops a republished event inside its 2 minute duplicate window.
- There is no outbox table and no processed-events table. The order row or reservation row is the record of what was done.

## Alternatives considered

- Transactional outbox with a relay: closes the commit/publish gap, but adds a table, a relay process and polling to every service.
- Exactly-once delivery: not available end to end across a database and a broker.
- Processed-events table: a second source of truth that duplicates what the business row already says.

## Consequences

- The gap is repaired by redelivery, not prevented: after a crash between commit and publish, the retried message re-publishes the stored outcome.
- If the redelivery arrives after the duplicate window, a consumer may see a duplicate. Consumers are idempotent for the same reason, so this is harmless.
- A publish from `POST /orders` that times out after the broker accepted the message still deletes the order row while the saga continues: the order is missing for the client, the result lands in `dlq.orders.*`, and the stock stays reserved until the next restock. The outbox removes this too.
- Outbox is the planned fix for iteration 2 if the gap must be closed instead of repaired.
