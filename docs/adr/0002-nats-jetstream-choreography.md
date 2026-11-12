# ADR 0002: NATS JetStream and choreography for the order saga

## Status

Accepted

## Context

An order flows through three services: orders creates it, inventory reserves stock, payments charges the customer, and notifications sends an email. Each service owns its database, so there is no distributed transaction. We need a broker with persistence, retries and a dead-letter path, and a way to coordinate the steps that is easy to explain.

## Decision

- Use NATS JetStream as the broker. Four file-backed streams (`ORDERS`, `INVENTORY`, `PAYMENTS`, `DLQ`) keep messages for 7 days with a 2 minute duplicate window.
- Coordinate by choreography: each service reacts to events and emits its own. There is no orchestrator.
- Event IDs are deterministic (`uuid.NewSHA1(orderID, subject)`) and sent as `Nats-Msg-Id`, so republishing is deduplicated.
- Consumers retry up to 5 times with growing delays, then move the message to `dlq.<durable>.<subject>`.

## Alternatives considered

- RabbitMQ: mature and flexible routing, but retries with delays and replay need extra plugins or queue topologies.
- Kafka: strong ordering and replay, but heavy to run and operate for five small services.
- Orchestration (a central saga coordinator): one place to read the flow, but it adds a service and couples every step to it.

## Consequences

- One small binary with built-in persistence, deduplication and per-message acks runs locally and in Compose.
- The saga flow lives in the services' subscriptions, so it must be documented to be understood end to end.
- Handlers must be idempotent: they store their outcome under the order ID and re-publish it on redelivery.
- Delivery is at least once; without an outbox a crash between commit and publish is repaired only by redelivery.
