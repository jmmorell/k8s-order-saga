# Design: Kubernetes Portfolio — async order saga, delivered in one week

Date: 2026-11-12 (v2 — rescoped to one week; supersedes v1)

## 1. Intent

A portfolio project for an author targeting **Senior Engineering Manager / Technical Product Manager** roles. It must show **technical credibility with Kubernetes** (stood up, operated and monitored a cluster) and, above all, **judgment**: decisions, trade-offs, cost, risk, and what was deliberately left out.

The story the repo tells: *"I directed AI agents to deliver a realistic platform in one week; here are the decisions I made and why."* The application is a pretext. The written artifacts (ADRs, diagram, cost table, postmortems, roadmap) are first-class deliverables, not documentation chores.

Legend: **[Decided]** by the author · **[Proposal]** designer's call, revisable · **[Builder]** left to whoever implements.

## 2. Goals, non-goals, anti-goals

**Goals**
- Several Go services communicating **asynchronously, pure choreography**, over NATS JetStream. [Decided]
- Runs **locally first** (kind), then **once on GCP** (GKE) with evidence captured, then destroyed. [Decided]
- **OpenTelemetry** observability: a trace that crosses the messages, metrics, a dashboard, an SLO, alerts. [Decided]
- **Security basics**: only what is necessary is exposed; default-deny networking; hardened pods. [Decided]
- **Deploy from Git**: manually triggered workflow + Argo CD. [Decided]
- **Operate it**: two failure drills with written postmortems. [Decided]
- Everything in the repo is in **English**. [Decided]
- Done in **7 days × 4 h** of the author's time. [Decided]

**Non-goals (this iteration)** — see §11 for the iteration-2 roadmap
- Private VPC, own domain + HTTPS certificate, image signing, outbox pattern, Terraform-created project, Secret Manager + External Secrets, service mesh.
- Real payments, UI, end-user auth, HA, backups.

**Anti-goals**
- Time going into the application instead of the platform and the narrative.
- Pieces the author cannot explain.
- Secrets in the repo; internal services exposed to the internet.
- A surprise GCP bill (cluster left running).
- Hiding that agents wrote the code — the README states it openly.

## 3. Constraints

- Go (latest stable) for services. Single Go module, monorepo, public GitHub repo. [Decided]
- Author's machine: Mac M4 Pro, 24 GB (Apple Silicon → images must be multi-arch `linux/amd64` + `linux/arm64`).
- Author knows GCP tooling and the terminal; has not configured Kubernetes hands-on before.
- GCP only on demand, smallest machines that hold the stack. Accepted cost: ~0.12–0.16 $/h while up. [Decided]

## 4. Who does what [Decided]

| Part | Who |
|---|---|
| Go application, OTel instrumentation, CI workflow | Agents; author reviews, runs and can explain it |
| Platform (kind, Helm, NetworkPolicies, Argo CD, Terraform) | **Author, guided step by step**; if a day runs slow, agents finish and the author reviews |
| ADRs, postmortems, README | Author with assistance |

Each platform day starts with a short session guide (written that day, informed by the previous day). Each day ends with something demonstrable and a README section.

## 5. The application

Services (all Go, one distroless image each):

| Service | Role | Exposed | DB |
|---|---|---|---|
| `orders` | HTTP API `POST /orders`, `GET /orders/{id}`; closes the saga | **Yes** (only one) | Yes |
| `inventory` | Reserves / rejects / releases stock | No | Yes |
| `payments` | Simulated charge; `PAYMENT_FAILURE_RATE`, `PAYMENT_LATENCY_MS` | No | Yes |
| `notifications` | Emails result to Mailpit (internal) | No | No |
| `loadgen` | Continuous order traffic | No | No |

Saga (no orchestrator):

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

Reliability model (simplified from v1) [Proposal]:
- **At-least-once delivery + idempotent handlers.** Each handler stores its outcome under a natural key (`order_id`) and publishes **after commit**. On redelivery it re-publishes the stored outcome.
- **Deterministic event IDs** (`UUIDv5(order_id, subject)`) + JetStream dedup (`Nats-Msg-Id`), so a re-publish is harmless.
- **Dead-letter queue** after 5 deliveries.
- **No outbox.** Known limitation: a crash between commit and publish in `orders` leaves an order `PENDING` with no event. The "stuck PENDING" alert detects it. Documented in an ADR as the iteration-2 fix.
- One logical Postgres database per service; no cross-database reads.
- W3C `traceparent` travels in NATS headers (added on day 3).

## 6. Platform

**Local** [Proposal unless noted]
- **kind**, 1 control plane + 2 workers; default CNI (kindnet enforces NetworkPolicies in current kind releases).
- Entry: **Gateway API** with **Envoy Gateway** (ingress-nginx is retired).
- **Helm** [Decided]: one generic chart for the Go services; upstream charts for NATS, observability, Envoy Gateway, Argo CD. Postgres: a simple StatefulSet with the official image (easiest to explain).
- One command up, one command down.

**Observability** [Decided: OpenTelemetry]
- OTel SDK in the services → **OTel Collector** → **Tempo** (traces), **Prometheus** (metrics, via kube-prometheus-stack), **Loki** (logs — first thing to drop if short on time).
- **Grafana**, dashboards as code: one saga dashboard (created → confirmed/cancelled, latency, DLQ, consumer lag).
- One **SLO** (e.g. 99 % of orders reach a final state within 5 s) and alerts: payment error rate, DLQ > 0, orders stuck `PENDING`.
- Collector exporter is swappable to Cloud Trace / Cloud Monitoring without touching service code; documented, not implemented. [Decided]

**Security** [Decided scope]
- Only `orders` is reachable from outside. Grafana, Argo CD, NATS, Postgres, Mailpit via `kubectl port-forward`.
- **NetworkPolicies default-deny** per namespace with explicit allows.
- **Pod Security Standards `restricted`**: non-root, read-only root filesystem, no privilege escalation, all capabilities dropped, seccomp `RuntimeDefault`.
- One ServiceAccount per service, no API permissions, token not mounted.
- Requests/limits everywhere.
- No secrets in Git: local Secrets generated by the bootstrap script; on GCP created by Terraform.
- **Trivy** scan in CI (fails on fixable HIGH/CRITICAL).

**GitOps** [Decided]
- **Argo CD** in-cluster, app-of-apps, pulls from the public repo. CI never touches the cluster.
- GitHub Actions `workflow_dispatch`: test → build multi-arch → Trivy → push to **GHCR** → commit the new image tag to the Helm values → Argo CD syncs.
- Cheap checks (lint, unit tests) on every push.

**GCP** [Decided: GKE + Cloud SQL, on demand]
- Manual one-time prerequisites, documented: GCP project, billing link, **budget alert**.
- **Terraform**, single root module, remote state in GCS: VPC + subnet, **GKE Standard zonal**, **3 × `e2-standard-2` spot**, Dataplane V2, Workload Identity; **Cloud SQL PostgreSQL** smallest tier, reached through the **Cloud SQL Auth Proxy** sidecar with Workload Identity.
- Entry: GKE Gateway on a public IP over HTTP (no domain this iteration).
- `make gcp-up` → capture evidence (screenshots, trace, dashboard, `kubectl` output) → `make gcp-down`.
- **Plan B** if Cloud SQL wiring exceeds half the day: in-cluster Postgres on GKE too; Cloud SQL moves to the roadmap with an ADR.

## 7. Schedule (4 h/day)

| Day | Deliverable | Who |
|---|---|---|
| 1 | App + Docker Compose; saga incl. compensation and DLQ; smoke test | Agents |
| 2 | kind + Helm; `orders` reachable through the Gateway | Author, guided |
| 3 | OTel end-to-end; saga trace, dashboard, SLO, alerts | Agents; author runs it |
| 4 | NetworkPolicies + pod hardening; Argo CD + manual workflow | Author + agents |
| 5 | GKE + Cloud SQL with Terraform; up, evidence, down | Author, guided |
| 6 | Two failure drills with postmortems; KEDA autoscaling on NATS lag if time allows | Author |
| 7 | README, architecture diagram, cost table, roadmap | Author, assisted |

**Cut order if behind schedule** (first to go first): KEDA autoscaling → Loki → Cloud SQL (use plan B) → CI tag-bump (Argo CD still syncs manual commits) → second failure drill.

## 8. Failure drills (day 6) [Proposal]

Run on kind. Each produces `docs/postmortems/NNNN-<title>.md` (timeline, detection, root cause, fix, follow-ups).
1. **Payment outage**: raise `PAYMENT_FAILURE_RATE` → alert fires → find it in the trace → confirm compensations restored stock.
2. **Bad rollout**: ship an image that fails readiness → rollout stalls safely → roll back with `git revert` through Argo CD.
Spare: drain/kill a worker node and watch pods reschedule.

## 9. Portfolio presentation (day 7)

README, readable in one minute: what it is, architecture diagram, a saga trace screenshot, the dashboard, **decisions table** linking to ADRs, **cost table**, the two postmortems, "how this was built with AI agents and what I owned", and the iteration-2 roadmap.

ADRs (short; written before each piece): monorepo/single module; NATS + choreography; at-least-once + idempotency instead of outbox; database per service, Cloud SQL on GCP vs in-cluster locally; Gateway API; observability stack and swappable exporter; GitOps pull model with manual trigger; scope cuts for a one-week delivery.

## 10. Left to the builder

Everything marked [Builder], plus: exact directory layout, Go libraries beyond the natural starting set (`nats.go`, `pgx`, `goose`, OTel SDK, `slog`), chart versions, resource names, Makefile/script shape. Between equivalent options, **prefer the one that is easiest to explain**.

## 11. Iteration-2 roadmap (explicitly out of scope now)

| Item | Why deferred |
|---|---|
| Private VPC: private nodes, Cloud NAT, Cloud SQL private IP, restricted control plane | Largest GCP effort; design already avoids CI→cluster access so it is additive |
| Own domain + Google-managed certificate | Purchase + DNS delegation + provisioning time |
| Outbox pattern | Closes the commit/publish gap; detection alert covers it meanwhile |
| cosign signing + in-cluster verification | Trivy covers the highest-value check first |
| Terraform-created project, two-layer Terraform (`foundation` / `environment`) | Needs billing-admin flow; manual prerequisite is documented |
| Secret Manager + External Secrets Operator | Terraform-created Secrets are enough for an ephemeral cluster |
| Switch exporter to Cloud Operations | Documented path; same stack kept for consistency |
