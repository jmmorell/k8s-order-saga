# k8s-portfolio

Five event-driven Go services (orders, inventory, payments, notifications, load generator) built as a choreography over NATS JetStream.

## Delivery semantics

Events are delivered at least once. `notifications` has no database, so a
redelivered `orders.confirmed` or `orders.cancelled` event can send a
duplicate email.
