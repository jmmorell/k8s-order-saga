# ADR 0001: Monorepo with a single Go module

## Status

Accepted

## Context

The project consists of five small services that share platform code: configuration, logging, health endpoints, the event envelope and NATS helpers. We need a layout that is easy to build, test and explain.

## Decision

Keep all services in one repository and one Go module named `github.com/jmmorell/k8s-order-saga` at the repo root.

- `cmd/<service>` holds each service entry point.
- `internal/platform/...` holds shared packages (config, logx, httpx, ...).
- `internal/<service>` holds service-specific code.

## Alternatives considered

- One module per service with a `go.work` workspace: stronger isolation of dependencies, but more files, versioning of the shared code and a more complex build for little benefit at this size.
- One repository per service: maximum independence, but shared code would need to be published and versioned separately.

## Consequences

- A single `go.mod`, one `go test ./...` and one lint run cover everything.
- Shared code changes are atomic across all services.
- All services share one dependency set; a dependency upgrade affects every service at once.
- Services can be split into separate modules later if independence becomes necessary.
