# Backlog — deferred findings from the reviews

Minor findings the task and final reviews raised and deliberately left open, grouped by the day that found them. None blocks the day after it.

## Messaging (`internal/platform/events`)
- Cancelling the context passed to `Consume` does not stop the subscription; callers must call `stop()`.
- `stop()` does not wait for an in-flight handler (documented in the README).
- The default pull buffer can exceed `AckWait` when handlers are slow, burning delivery attempts.
- An empty `Subjects` list consumes the whole stream; it should be a validation error.
- DLQ publishes carry no `Nats-Msg-Id`, so a failed `Term` after a successful DLQ publish duplicates the DLQ entry.
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

## Day 2
- `make dlq` / `make local-dlq` need `python3`.
- `scripts/chart-test.sh` greps the whole render instead of the Deployment document, and its negative test does not check the failure message.
- `deploy/local/versions.env` lists `POSTGRES_IMAGE` / `MAILPIT_IMAGE` that nothing reads (the tags are duplicated in the manifests).
- The Postgres password is passed with `--from-literal` (visible in argv); shellcheck is not run on the scripts.
- The kind node-image check is whitespace-fragile and hard-codes three nodes (it fails closed).
- `local-smoke` / `local-dlq` port-forwards fail silently if the local port is taken.
- The `reload` branch of `local-up.sh` skips `check_prereqs`.
- The `nats` readiness check is connection-level; a JetStream-level check would be more truthful.
- Services open their HTTP listener only after dependencies are reachable; starting the listener first would make `/healthz` available immediately and remove start-up restarts entirely.
- Readiness depends on NATS for every service, so during a NATS outage both `orders` replicas leave the Service and even `GET /orders/{id}` (Postgres only) is unavailable. Alternative: readiness means "can serve my HTTP API" and `POST` returns 503 by itself. A deliberate simplification to revisit.
- No `topologySpreadConstraints` or PodDisruptionBudget for the two `orders` replicas.
- No `GOMEMLIMIT` derived from the memory limit.

## Day 3
- Telemetry setup: the 1 s bound in `TestSetup_UnreachableCollectorDoesNotBlock` may flake on a slow machine; the five `cmd/*/main.go` repeat the same telemetry setup/shutdown boilerplate.
- Tracing: `consume.go` repeats the messaging attribute literals that `messagingAttrs` in `tracing.go` already builds; loadgen's outgoing `traceparent` has no test.
- `PROMETHEUS_IMAGE` (promtool for `make rules-test`) must match the Prometheus version kube-prometheus-stack deploys, but nothing enforces it; the image tags of the charts' sub-components are pinned only through the chart versions.
- SLO rule tests lack a 5 % slow-burn case (only the 20 % case fires both alerts) and value checks for the 30m and 6h ratios.
- `obs-check.sh` and `trace-check.sh` wait for their port-forward by polling the local port, which also succeeds against a stale listener left on that port; `obs-check.sh` does not abort when Prometheus never becomes ready (the queries then fail one by one).
- Loki's retention is set to 6 h, below the 24 h index period that Loki's compactor retention is designed around; it may not be honoured exactly (data can live longer than 6 h). The `emptyDir` bounds it anyway.
- Loki indexes `k8s_pod_name` as a stream label (its default OTLP mapping); fine locally, but every pod restart creates new streams. Moving it to structured metadata would be the choice under pod churn.
- `OrdersStuckPending` can fire after a NATS outage longer than 2 minutes: orders published but not finished when NATS stopped stay PENDING until it returns. They complete by themselves, so the page is noise in that case.
- `obs-check.sh` and `trace-check.sh` also need `python3` on the host (like `make local-dlq`).
- `EventsDeadLettered`: its second term (`or ... unless ... offset 10m`) also fires once, about 10 minutes after a Collector outage or a Prometheus restart, while any `dead_letter > 0` series exists (the tour section 1 drill scales the Collector to 0).
- The dashboard "dead-lettered/s" panel misses a pod's first dead letter for the same reason (no previous sample to take a rate from); the DLQ size line covers it.
- kube-prometheus-stack's default `CPUThrottlingHigh` fires for the Grafana dashboard sidecar (routed to the null receiver, so no email); its 100m CPU limit is tight. (It also fired for node-exporter, which Day 4 disabled, ADR 0009.)
- The `reason` label of `saga.orders.completed` comes from the event payload unvalidated; it is bounded in practice (only our constants are ever published) but could be clamped to the known set.

## Day 4
- `scripts/policy-test.sh` waits a fixed `sleep 5` after its probe pods are Ready to get past kindnet's startup window (ADR 0009); it is timing-based and could flake on a slow machine.
- A probe pod left over from an aborted `make policy-test` run (killed before its EXIT trap) is not cleaned up before the next run, which then fails to create a pod of the same name.
- `release.yml` pushes a rebuild of what it scanned (served from the build cache, but not the scanned artifact itself), and the base images (`golang`, distroless) are referenced by tag, not by digest.
- The `image` job of `release.yml` holds `packages: write` on pull-request runs too, although only dispatches push; a separate dispatch-only push job would narrow it.
- CI runs twice per commit on a branch with an open pull request (`push` and `pull_request`).
- `apply_root_app` writes `ARGOCD_REVISION` unquoted into the root Application with `sed`; the revision is validated against `^[A-Za-z0-9._/-]+$`, but a value YAML reads as a number or boolean would still be mis-typed.
- `check_no_helm_workload` passes silently if `helm list` itself fails, so the migration guard can be skipped.
- Postgres, Mailpit and NATS run under their namespace's `default` ServiceAccount (token not mounted); dedicated ServiceAccounts would make the identity explicit.
- The observability charts (OTel Collector and Tempo fail `restricted`), Envoy Gateway and Argo CD stay at `baseline` enforce (ADR 0009).
- The platform charts (Postgres, NATS, Mailpit, the observability stack, Envoy Gateway) are not yet managed by Argo CD (ADR 0010).
- The platform images (Postgres, NATS, Mailpit) are not scanned by any workflow and are pinned by moving tags (`postgres:17-alpine`).
- A blocked SMTP path dead-letters only after tens of minutes: notifications handles one message at a time with a 10 s dial timeout, and its client buffer holds about 300 messages, so a message's fifth attempt comes round slowly. Emails stop at once; `EventsDeadLettered` is late.
- After a payments outage of a few minutes, recovery takes about twice as long: payments pulls hundreds of events into its client buffer, handles them one at a time, and those held past the 30 s `AckWait` are redelivered and sleep `PAYMENT_LATENCY_MS` again before the idempotency check (see the payments item above). A smaller pull batch or `MaxAckPending` would bound it.
- Pre-existing: the API server logs TLS errors calling Envoy Gateway's `envoy-gateway-topology-injector` webhook (`x509: certificate signed by unknown authority`).
