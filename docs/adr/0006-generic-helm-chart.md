# ADR 0006: One generic Helm chart for the Go services

## Status

Accepted

## Context

The five Go services (orders, inventory, payments, notifications, loadgen) are deployed to a local kind cluster. They are built from the same Dockerfile, read their configuration from environment variables and expose the same `/healthz` and `/readyz` endpoints. Their Kubernetes objects differ only in name, replica count and environment.

## Decision

- A single chart, `charts/service`, deploys any one of them. The release name is the service name, so `helm install orders charts/service` creates a Deployment, Service and ServiceAccount called `orders`.
- Per-service differences live in `deploy/local/values/<service>.yaml`: replicas, plain environment variables and references to Secret keys.
- The chart owns everything that must be identical across services: the hardened security context, probes, resource requests and limits, and `HTTP_ADDR`. A service cannot opt out of them.
- `image.repository` is required and has no default, so a forgotten value fails at render time instead of pulling a wrong image.
- Secret references are rendered before plain variables, so a value can use Kubernetes `$(POSTGRES_PASSWORD)` expansion and no password is written to Git.
- The chart contains only application workloads. NATS, PostgreSQL, Mailpit and Envoy Gateway are third-party components with their own pinned charts and manifests, installed by the bootstrap scripts into `platform` and `envoy-gateway-system`. Mixing them in would tie their lifecycle to the services and duplicate upstream configuration.

## Alternatives considered

- A chart per service: explicit, but five near-identical copies drift apart, and a security fix must be repeated five times.
- Kustomize with a shared base and one overlay per service: no templating language, but names, labels and selectors must be patched in every overlay, which is harder to read than a values file.
- Plain manifests: simplest to start, but the most repetition.

## Consequences

- A change to probes, security settings or resources is made once and applies to all services.
- The chart is deliberately small. Anything only one service needs becomes a chart feature or a reason to reconsider this decision.
- The chart can be linted, rendered and validated offline with `make chart-test`, with no cluster.
