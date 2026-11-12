# ADR 0004: One logical database per service

## Status

Accepted

## Context

Orders, inventory and payments each keep their own state. Sharing tables would be simpler to query, but it would couple the services through their schemas and hide the events that are supposed to be the contract between them.

## Decision

- Each service that needs storage owns one logical database: `orders`, `inventory`, `payments`.
- In Compose they live on a single PostgreSQL server to keep the stack light. In Kubernetes each can move to its own server without code changes.
- A service connects only to its own database. There are no cross-database reads or joins; data crosses a boundary only as an event.
- Each service embeds its SQL migrations and applies them with goose at startup, so a fresh database becomes usable without a separate step.

## Alternatives considered

- One shared schema: easy to query, but any schema change can break another service.
- One server per service: strongest isolation, but costly to run locally for a demo.
- Separate migration job: cleaner for large teams, but one more moving part to explain.

## Consequences

- Services can be changed and deployed independently of each other's schemas.
- Anything one service needs from another must be carried in an event payload, for example the amount in `inventory.reserved`.
- Several replicas migrating at once is safe because goose takes an advisory lock.
