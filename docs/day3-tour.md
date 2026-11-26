# Day 3 tour: seeing inside the saga

Day 2 put the saga on Kubernetes. Day 3 made it observable: every service now sends traces, metrics and logs, a small stack stores them, Grafana shows them, and an alert reaches a (fake) on-call inbox when something goes wrong. You did not write this code. This tour is how you come to own it: for each piece you see what it is, which file defines it, how to look at it live, and (where it is safe) how to break it and watch.

> **Since day 4** ([day-4 tour](day4-tour.md)), a few things here read differently. Argo CD deploys the five services and `saga-observability` from Git, so `helm list` no longer shows them (section 1 lists four releases in `observability`: `kps`, `loki`, `otel-collector` and `tempo`), and the payment-outage drill in section 6 cannot use `helm upgrade payments`: `payments` is not a Helm release any more, and a `kubectl` change to it is reverted within seconds. Pause Argo CD first and change the Deployment with `kubectl --context kind-order-saga set env deployment/payments -n shop PAYMENT_FAILURE_RATE=1`; resuming Argo CD is the restore (day-4 tour, section 4, "Pausing Argo CD"). node-exporter is disabled (ADR 0009), so section 1 shows no `kps-prometheus-node-exporter-...` pods and section 8's first item is already gone.

You need the cluster running with some minutes of loadgen traffic behind it (`make local-up`, then `make local-smoke` prints `SMOKE OK` and `make obs-check` prints `OBS OK`). As on day 2, every command passes `--context kind-order-saga` (or `--kube-context` for Helm), the commands are written for zsh and bash, and you run them from the repository root.

Nothing in the observability stack is reachable from outside the cluster. You reach it with `kubectl port-forward`, a temporary tunnel through the API server that lives as long as the command runs. This tour opens several of them. Each one runs forever, so start it in a **second terminal** (a third one helps too) and stop it with Ctrl-C when the section is done. The local ports used are 9090 (Prometheus), 3000 (Grafana), 9093 (Alertmanager), 8025 (Mailpit), 3100 (Loki), 3200 (Tempo) and 4222 (NATS); if one is taken, stop whatever holds it first.

## A few words first

Observability means being able to answer "what is the system doing, and why?" from the outside, without adding new code each time you ask. Three kinds of data, called **signals**, do the work:

- A **trace** is the story of one request through every service it touched. It is made of **spans**: one span per unit of work (an HTTP request, a SQL statement, publishing a message, handling a message), each with a start, a duration, a name, attributes and a parent. All spans of one request share a **trace id**. **Context propagation** is how the trace id travels between processes: the sender writes it into the message (an HTTP header or a NATS header called `traceparent`), the receiver reads it and makes its own spans children of the sender's.
- A **metric** is a number measured over time, cheap to store and to aggregate. A **counter** only goes up (orders created). A **histogram** counts observations into buckets (how many orders finished within 0.1 s, 0.25 s, ... 5 s ...), which is how you get percentiles. A **gauge** is a value that goes up and down (orders stuck right now).
- A **log** is one line of text with fields. Ours are JSON and now also carry the `trace_id` of the span they were written in.

**OpenTelemetry** (OTel) is the open standard for producing all three, and **OTLP** is its wire protocol. The Go services use the OTel SDK; they do not know where the data ends up.

## 1. The three signals and the path each one takes

**What it is.** Every service pushes all three signals over OTLP/HTTP to one **OpenTelemetry Collector**. The Collector is a router: it receives, buffers, batches and forwards each signal to the backend that stores it. Grafana is the single window onto all of them.

```
service (OTel SDK) ──OTLP/HTTP :4318──► otel-collector (observability)
                                          │
   traces  ── otlp_grpc ──────────────────┼──► Tempo :4317           ─┐
   metrics ── prometheus exporter :8889 ◄─┼── scraped every 15 s by   │
                                          │     Prometheus ──► rules ──► Alertmanager ──► Mailpit
   logs    ── otlp_http ──────────────────┴──► Loki :3100/otlp       ─┤
                                                                      └──► Grafana (all four as data sources)
```

The services still write their JSON logs to stdout as before, so `kubectl logs` keeps working; the OTLP copy is what lands in Loki. NATS is the one platform piece that is scraped directly (a `prometheus-nats-exporter` sidecar in `nats-0`), for the DLQ size and consumer backlog.

**Why a Collector in the middle.** The services know one address. Which backends exist, how data is batched, how much memory the pipeline may use, and whether a copy also goes to Google Cloud is the Collector's configuration, not the services' code (ADR 0008).

**Defined in:**

| Piece | File |
|---|---|
| SDK setup (exporters, propagator) | `internal/platform/telemetry/telemetry.go` |
| `OTEL_*` environment for each service | `charts/service/templates/deployment.yaml`, value `otel.endpoint` in `deploy/local/values/<service>.yaml` |
| Collector pipelines | `deploy/local/values/otel-collector.yaml` |
| Tempo, Loki | `deploy/local/values/tempo.yaml`, `deploy/local/values/loki.yaml` |
| Prometheus, Alertmanager, Grafana, data sources | `deploy/local/values/kube-prometheus-stack.yaml` |
| Dashboard, SLO and alert rules | `charts/saga-observability/` |
| Install order and waits | `scripts/local-up.sh` (function `install_observability`) |
| Versions | `deploy/local/versions.env` |

See what runs in the new namespace:

```bash
kubectl --context kind-order-saga get pods -n observability
```

You should see, all `Running`: `alertmanager-kps-alertmanager-0` (`2/2`), `kps-grafana-...` (`3/3`: Grafana and two sidecars), `kps-kube-state-metrics-...`, `kps-operator-...`, three `kps-prometheus-node-exporter-...` (one per node), `loki-0`, `otel-collector-...`, `prometheus-kps-prometheus-0` (`2/2`) and `tempo-0`.

```bash
helm --kube-context kind-order-saga list -n observability
```

You should see five releases: `kps` (kube-prometheus-stack), `loki`, `otel-collector`, `saga-observability` (our dashboard and rules) and `tempo`. A sixth, `metrics-server`, lives in `kube-system`.

How a service learns where to send its data:

```bash
kubectl --context kind-order-saga get deployment orders -n shop -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' | grep -E '^(OTEL|POD)_'
```

You should see `POD_NAME=` and `POD_NAMESPACE=` (empty here because they come from the downward API, filled in per pod), `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability.svc.cluster.local:4318`, `OTEL_SERVICE_NAME=orders`, `OTEL_RESOURCE_ATTRIBUTES=k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE)` and `OTEL_METRIC_EXPORT_INTERVAL=15000` (push metrics every 15 s). In Docker Compose `otel.endpoint` is empty and none of this is set, so nothing is exported.

The Collector's three pipelines, as the chart rendered them:

```bash
kubectl --context kind-order-saga get configmap otel-collector -n observability -o jsonpath='{.data.relay}' | sed -n '/pipelines:/,/telemetry:/p'
```

You should see `logs`, `metrics` and `traces`, each with the receiver `otlp`, the processors `memory_limiter` then `batch`, and one exporter: `otlp_http/loki`, `prometheus` and `otlp_grpc/tempo`. (The last line, `telemetry:`, starts the Collector's settings for its own metrics.) `memory_limiter` refuses data when the Collector nears its memory limit instead of being killed; `batch` groups data into fewer, larger requests.

Prometheus finds what to scrape through ServiceMonitor and PodMonitor objects, which the Prometheus Operator turns into scrape configuration:

```bash
kubectl --context kind-order-saga get servicemonitor,podmonitor -A | grep -E 'otel-collector|nats'
```

You should see `servicemonitor.../otel-collector` in `observability` and `podmonitor.../nats` in `platform`. The other ServiceMonitors (`kps-...`) come with kube-prometheus-stack and watch the cluster itself.

Now ask Prometheus whether it can reach both. In the second terminal:

```bash
make prom
```

You should see `Forwarding from 127.0.0.1:9090 -> 9090`. Leave it running; sections 4 to 7 need it. (`http://localhost:9090` in a browser is Prometheus' own UI, if you want to type queries there.) Back in the first terminal, define a small helper that runs one PromQL query and prints each result as labels and value:

```bash
promq() { curl -s localhost:9090/api/v1/query --data-urlencode "query=$1" | python3 -c 'import json,sys; [print(r["metric"], r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'; }
```

You should see nothing; the function now exists in this shell. Use it:

```bash
promq 'up{job=~"otel-collector|platform/nats"}'
```

You should see two lines ending in `1`, one with `'job': 'otel-collector'` and one with `'job': 'platform/nats'`. `up` is 1 when the last scrape worked. Note what is missing: there is no `up` for `orders` or `payments`, because Prometheus never scrapes the services; they push to the Collector (ADR 0008 explains what that costs).

### Break it and watch: take the Collector away

The Collector is a single point of loss for telemetry, but not for the saga. Prove it. Scale it to zero:

```bash
kubectl --context kind-order-saga scale deployment otel-collector -n observability --replicas=0
```

You should see `deployment.apps/otel-collector scaled`. Orders keep flowing:

```bash
make local-smoke
```

You should see `SMOKE OK`. The services only complain, in their stdout:

```bash
kubectl --context kind-order-saga logs -n shop deploy/orders --since=1m | grep -m1 -E 'connection refused|no such host'
```

You should see `Found 2 pods, using pod/orders-...` and one line from the OTel SDK like `... Post "http://otel-collector.observability.svc.cluster.local:4318/v1/logs": dial tcp ...: connect: connection refused` (`/v1/traces` or `/v1/metrics` are just as likely). If it prints nothing, wait 15 seconds (the next metric push) and run it again. The telemetry of this window is lost; the orders are not.

**Restore.**

```bash
kubectl --context kind-order-saga scale deployment otel-collector -n observability --replicas=1 && kubectl --context kind-order-saga rollout status deployment/otel-collector -n observability --timeout=120s
```

You should see `deployment "otel-collector" successfully rolled out`. Within about 30 seconds `promq 'up{job="otel-collector"}'` prints one line ending in `1` again, with a new `pod` and `instance` (the new pod has a new IP).

## 2. One order as a trace

**What it is.** One order touches four services and three databases. Its trace shows every step with its timing, in one picture.

**Defined in** `internal/platform/events/publish.go` and `consume.go` (the `publish ...` and `process ...` spans and the `traceparent` header), `internal/platform/events/tracing.go` (the header carrier and the `order.id` attribute), `internal/orders/api.go` (the HTTP server span from the `otelhttp` library, plus the `order.id` attribute on it), `internal/platform/pgutil/pgutil.go` (the `otelpgx` tracer that makes one span per SQL statement) and `internal/loadgen/service.go` (loadgen's HTTP client, which sends a `traceparent` header). The SDK itself is started by `telemetry.Setup` in each `cmd/<service>/main.go`.

Place one order and keep its id:

```bash
ORDER_ID=$(make -s order | sed -E 's/.*"id":"([^"]+)".*/\1/'); echo "$ORDER_ID"
```

You should see a fresh order id (a UUID). `make order` posts the order, waits one second and reads it back; the `sed` keeps only the id.

Now find its trace. The script searches Tempo for spans whose `order.id` attribute is this id, checks that one trace covers all four services, then checks Loki for the matching log line:

```bash
scripts/trace-check.sh "$ORDER_ID"
```

You should see, after up to about 30 seconds (spans are batched in the SDK and again in the Collector): `trace <32 hex digits>`, `services: inventory notifications orders payments`, `TRACE OK`, `log: order created`, `LOGS OK`. Keep the trace id; the second run is quick because everything has arrived:

```bash
TRACE_ID=$(scripts/trace-check.sh "$ORDER_ID" | sed -n 's/^trace //p'); echo "$TRACE_ID"
```

You should see the same trace id.

### Open it in Grafana

In the second terminal (stop `make prom` with Ctrl-C first, or use a third terminal):

```bash
make grafana
```

You should see `Grafana on http://localhost:3000, user admin. Password:` followed by the `kubectl` command that prints the password, then `Forwarding from 127.0.0.1:3000 -> 3000`. The password is generated by `local-up.sh` into the Secret `grafana-admin` and is never in Git; run the printed command in the first terminal to read it. Open `http://localhost:3000`, log in as `admin`, then:

1. Open **Explore** (compass icon in the left menu) and pick the **Tempo** data source at the top.
2. Choose the **TraceQL** query type and run `{ span.order.id = "<your order id>" }`. (Or paste the trace id itself into the query field.)
3. Click the trace in the result table. It opens as a waterfall: one row per span, indented under its parent, a bar for its duration.

The dashboard **Order saga** (Dashboards menu) has the same recipe in its last panel.

### Read the spans

You can see the same tree from the terminal. Open a tunnel to Tempo in the second terminal:

```bash
kubectl --context kind-order-saga port-forward -n observability svc/tempo 3200:3200
```

You should see `Forwarding from 127.0.0.1:3200 -> 3200`. Then print the trace, one span per line in start order:

```bash
curl -s -H 'Accept: application/json' "localhost:3200/api/traces/$TRACE_ID" | python3 -c '
import json, sys
spans = []
for rs in json.load(sys.stdin)["batches"]:
    svc = next(a["value"]["stringValue"] for a in rs["resource"]["attributes"] if a["key"] == "service.name")
    for ss in rs["scopeSpans"]:
        spans += [(int(s["startTimeUnixNano"]), svc, s["kind"].replace("SPAN_KIND_", ""), s["name"]) for s in ss["spans"]]
t0 = min(s[0] for s in spans)
for start, svc, kind, name in sorted(spans):
    print(f"{(start - t0) / 1e6:7.1f} ms  {svc:14} {kind:9} {name}")'
```

You should see about 28 lines like these for a confirmed order. A declined order has more (payments publishes `payments.failed`, which both orders and inventory consume, and inventory releases the stock and publishes `inventory.released`), and its last email says cancelled.

| Offset | Service | Kind | Span | What it is |
|---|---|---|---|---|
| 0 ms | orders | SERVER | `POST /orders` | The HTTP request (otelhttp). The root, and it carries `order.id`. |
| ~1 ms | orders | CLIENT | `pool.acquire`, `INSERT` | Taking a database connection and saving the PENDING order (otelpgx). |
| ~5 ms | orders | PRODUCER | `publish orders.created` | Storing the event in JetStream. |
| ~5 ms | inventory | CONSUMER | `process orders.created` | inventory handling that event: a child of the publish span, in another process. |
| | inventory | CLIENT | `BEGIN`, `SELECT`, `SELECT`, `UPDATE`, `INSERT`, `COMMIT` | Reserving stock in one transaction. |
| ~8 ms | inventory | PRODUCER | `publish inventory.reserved` | |
| ~8 ms | payments | CONSUMER | `process inventory.reserved` | Note the gap of about 200 ms before its first SQL span: that is `PAYMENT_LATENCY_MS`, the simulated card charge. payments handles one message at a time, so if it was busy with loadgen's previous order this span also starts up to 200 ms late. |
| | payments | CLIENT | `BEGIN`, `INSERT`, `SELECT`, `COMMIT` | Recording the payment. |
| ~220 ms | payments | PRODUCER | `publish payments.succeeded` | |
| ~220 ms | orders | CONSUMER | `process payments.succeeded` | orders closing the order: `SELECT`, `UPDATE`, `COMMIT`. |
| ~223 ms | orders | PRODUCER | `publish orders.confirmed` | |
| ~224 ms | notifications | CONSUMER | `process orders.confirmed` | The email. Sending over SMTP has no span of its own. |

Four span kinds tell you who is talking to whom: SERVER (someone called me over HTTP), CLIENT (I called a database), PRODUCER (I sent a message), CONSUMER (I received one). The whole saga takes about a quarter of a second, almost all of it the simulated payment.

`make order` also did a `GET /orders/<id>`; that request is a separate trace, because nothing links it to the POST. Orders placed by loadgen look slightly different: loadgen sends a `traceparent` with its POST, so their root span is loadgen's CLIENT span `HTTP POST` and orders' `POST /orders` is its child.

### Where `traceparent` sits in a NATS message

The trace crosses from orders to inventory inside the NATS message itself. `publish.go` calls `otel.GetTextMapPropagator().Inject(...)` to write the current span's context into the message headers; `consume.go` calls `Extract(...)` on the other side before starting the `process` span. Look at it on the wire. In the second terminal:

```bash
kubectl --context kind-order-saga port-forward -n platform svc/nats 4222:4222
```

You should see `Forwarding from 127.0.0.1:4222 -> 4222`. In the first terminal, speak the NATS text protocol by hand: connect, subscribe to `orders.created` for three seconds, and print the first message with its headers:

```bash
(printf 'CONNECT {"headers":true,"verbose":false}\r\nSUB orders.created 1\r\n'; sleep 3) | nc localhost 4222 | grep -a -m1 -A3 '^HMSG'
```

You should see four lines: `HMSG orders.created 1 _INBOX.... <sizes>` (a message with headers), `NATS/1.0`, `Nats-Msg-Id: <event id>` (the deduplication key from day 1) and `traceparent: 00-<32 hex>-<16 hex>-01`. The `traceparent` value is the W3C Trace Context format: version `00`, the **trace id**, the id of the **parent span** (here the `publish orders.created` span) and flags (`01` = sampled). Paste the trace id into Grafana's Tempo search and you get that order's trace, started by loadgen. The subscription only listens; JetStream still delivers the message to inventory as usual.

Stop the NATS and Tempo port-forwards with Ctrl-C.

## 3. From the trace to its logs, and back

**What it is.** Every log line written while a span is active carries that span's `trace_id` and `span_id`. Loki stores them as **structured metadata** next to the line (not inside the text and not as an index label), so you filter on them with `| trace_id="..."`. Grafana links both ways: from a span to its logs, and from a log line to its trace.

**Defined in** `internal/platform/logx` (the slog handler that writes JSON to stdout and, through the OTel bridge, sends the same record over OTLP), `deploy/local/values/loki.yaml`, and the data source settings in `deploy/local/values/kube-prometheus-stack.yaml` (`tracesToLogsV2` on Tempo, `derivedFields` on Loki).

### In Grafana

1. In the trace you opened in section 2, click a span of `orders`. In the span details, use **Logs for this span**. Grafana opens Loki with the query `{service_name="orders"} | trace_id="<trace id>"` over the span's time ±5 minutes, and shows `order created` (and `order confirmed` if you picked the span that closed the order).
2. And back: in **Explore**, pick the **Loki** data source and run `{service_name="payments"} |= "payment"`. Expand any line. Among its fields is `trace_id`, with a link to **Tempo**: it opens the trace that line was written in.

### From the terminal

Open a tunnel to Loki in the second terminal:

```bash
kubectl --context kind-order-saga port-forward -n observability svc/loki 3100:3100
```

You should see `Forwarding from 127.0.0.1:3100 -> 3100`. Ask Loki for every service's lines that carry your trace id (`=~".+"` means "any service"):

```bash
curl -s -G localhost:3100/loki/api/v1/query_range --data-urlencode "query={service_name=~\".+\"} | trace_id=\"$TRACE_ID\"" --data-urlencode 'since=1h' | python3 -c '
import json, sys
rows = [(v[0], s["stream"]["service_name"], v[1]) for s in json.load(sys.stdin)["data"]["result"] for v in s["values"]]
for ts, svc, line in sorted(rows): print(svc, line)'
```

You should see the day-1 story, now found by trace id instead of grep: `orders order created`, `inventory stock reserved`, `payments payment succeeded`, `orders order confirmed`, `notifications email sent` (or `payment failed`, `order cancelled` and `stock released` if the order was declined).

And back, from a log line to a trace. Take the most recent declined payment:

```bash
curl -s -G localhost:3100/loki/api/v1/query_range --data-urlencode 'query={service_name="payments"} |= "payment failed"' --data-urlencode 'since=10m' --data-urlencode 'limit=1' | python3 -c '
import json, sys
for s in json.load(sys.stdin)["data"]["result"]: print(s["stream"]["trace_id"], s["values"][0][1])'
```

You should see one line: a 32-digit trace id followed by `payment failed`. Paste that id into Grafana's Tempo search and you see the declined order: after `process inventory.reserved` in payments, the trace continues with `publish payments.failed`, inventory's `process payments.failed` (releasing the stock) and orders closing it as cancelled.

The same `trace_id` is in the stdout JSON too, so `kubectl --context kind-order-saga logs -n shop deploy/payments --tail=5` shows `"trace_id":"..."` on each line, even if Loki is down.

**What Loki does not have.** Only what goes through the logger reaches Loki. A panic, the Go runtime's own messages, and the logs of NATS, Postgres, Envoy and the observability stack itself stay in `kubectl logs` only (ADR 0008, "Logs over OTLP rather than scraping container stdout"). Stop the Loki port-forward with Ctrl-C.

## 4. Metrics: from a counter in Go to a panel

**What it is.** The services count things in code; the SDK adds the counts up and pushes them to the Collector every 15 seconds; Prometheus scrapes the Collector every 15 seconds and stores one time series per distinct set of labels; Grafana panels run PromQL over those series.

**Defined in** `internal/orders/metrics.go` (orders created and completed, final-state latency, stuck PENDING), `internal/payments/metrics.go`, `internal/inventory/metrics.go` and `internal/platform/events/metrics.go` (events published and consumed). The dashboard is `charts/saga-observability/dashboards/saga.json`.

Follow one counter. In Go:

```bash
grep -n 'saga\.orders\.created\|createdCounter' internal/orders/metrics.go internal/orders/api.go
```

You should see the counter declared as `saga.orders.created` in `metrics.go` and `createdCounter.Add(r.Context(), 1)` in `api.go`, right after the event is published and just before the `202 Accepted`.

The Collector translates the name for Prometheus: dots become underscores, counters get `_total`, units become suffixes. The table of all saga metrics:

| In Go | In Prometheus | Labels |
|---|---|---|
| `saga.orders.created` | `saga_orders_created_total` | none |
| `saga.orders.completed` | `saga_orders_completed_total` | `status`, `reason` (absent for confirmed) |
| `saga.order.duration` (histogram, seconds) | `saga_order_duration_seconds_bucket` / `_sum` / `_count` | `status`, `le` |
| `saga.orders.stuck_pending` (gauge) | `saga_orders_stuck_pending` | none |
| `saga.payments` | `saga_payments_total` | `result` |
| `saga.inventory.operations` | `saga_inventory_operations_total` | `result` |
| `saga.events.published` / `.consumed` | `saga_events_published_total` / `saga_events_consumed_total` | `subject`; consumed also `durable`, `result` |

Every series also carries resource labels: `service_name`, `k8s_pod_name`, `k8s_namespace_name` and a few more. With Prometheus still forwarded (`make prom` in the second terminal; restart it if you stopped it for Grafana, and redefine `promq` if this is a new shell), look at the raw counter:

```bash
promq 'saga_orders_created_total'
```

You should see two lines, one per `orders` pod (different `k8s_pod_name`), each ending in the number of orders that pod accepted since it started. The two numbers can be very different: loadgen calls the `orders` Service directly and keeps its connection open, and a Service balances connections, not requests, so most of loadgen's orders land on one pod. Note also the labels: `service_name`, `k8s_pod_name` and `exported_job` describe the service, while `job`, `instance` and `pod` describe the Collector that Prometheus actually scraped. A raw counter is rarely what you want; it resets to 0 when a pod restarts, and it says nothing about "now".

```bash
promq 'sum(rate(saga_orders_created_total[5m]))'
```

You should see `{}` and a number close to `2`: orders per second. `rate(x[5m])` takes each series' increase over the last five minutes, divided by 300 seconds, and handles counter resets for you. `sum(...)` then adds the two pods together and drops their labels. This is the expression behind the first panel, **Orders created/s** (the panel uses `$__rate_interval`, which Grafana fills in from the zoom level).

**Why there is no `order_id` label.** Each distinct combination of label values is a separate series, which Prometheus keeps in memory and indexes. With an `order_id` label, every order would create a new series that is written once and never again: at 2 orders a second that is 7 200 new series an hour, and Prometheus' memory grows without limit. Labels must have a small, fixed set of values (`status`, `reason`, `result`). Questions about one order belong to traces and logs, which are built for that. That is why the `order.id` attribute exists on spans and on log lines, but never on a metric.

```bash
promq 'count(saga_orders_created_total)'
```

You should see `{}` and `2`: two series for this metric, however many orders pass.

The histogram is three metrics. `_count` counts orders, `_sum` adds up their durations, and `_bucket` counts how many took at most `le` seconds:

```bash
promq 'sum by (le) (rate(saga_order_duration_seconds_bucket[5m]))'
```

You should see eleven lines, `le` from `0.1` to `+Inf`, with growing values: almost every order is already counted at `le="0.25"`, because the saga takes about a quarter of a second. The bounds are written as floats (`5.0`, not `5`), so a query for `le="5"` finds nothing. The **Final-state latency** panel turns these buckets into p50, p95 and p99 with `histogram_quantile`.

The dashboard reaches Grafana as a ConfigMap, picked up by a Grafana sidecar that watches for the label `grafana_dashboard: "1"`:

```bash
kubectl --context kind-order-saga get configmap -n observability -l grafana_dashboard=1 -o name | grep saga
```

You should see `configmap/saga-observability-saga`. Editing the dashboard in the Grafana UI is lost on the next pod restart; the JSON in Git is the source of truth. `make obs-check` runs every panel's query (and every rule) against Prometheus and fails if one errors or returns nothing it should.

## 5. The SLO: "99 % of orders reach a final state within 5 s"

**What it is.** An **SLI** (service level indicator) is a measurement of what users care about, as a ratio of good events to all events. Here: orders that reached `CONFIRMED` or `CANCELLED` within 5 seconds, divided by all orders that reached a final state. An **SLO** (objective) is the target for it: 99 %. Note that a cancelled order is still a good event: the saga did its job and the customer got an answer quickly.

The **error budget** is the 1 % that may be bad: about 1 order in 100 may take longer than 5 s. Over 30 days at 2 orders a second, that is roughly 52 000 slow orders. As long as budget remains, nothing needs waking up for.

The **burn rate** is how fast you spend the budget, relative to spending it exactly in 30 days. Burn rate 1 means "on track to use all of it by the end of the month". Burn rate 14.4 means "at this pace the month's budget is gone in about two days" (30 days / 14.4); an hour at that pace eats 2 % of the month's budget.

**Multi-window burn-rate alerts** (from the Google SRE workbook) page on budget burn rather than on single bad minutes:

- `SagaSLOFastBurn` (critical): burn rate above 14.4 over the last hour **and** over the last 5 minutes, for 2 minutes. The long window says "this is significant", the short window says "and it is still happening" (so the alert stops soon after a fix, instead of staying red for an hour).
- `SagaSLOSlowBurn` (warning): burn rate above 6 over 6 hours **and** 30 minutes, for 15 minutes. Catches a slow leak that the fast alert never notices.

**Defined in** `charts/saga-observability/rules/saga.rules.yaml` (group `saga.slo`), tested by `charts/saga-observability/rules/saga.rules.test.yaml`, shipped as the PrometheusRule `saga-observability` by `templates/prometheusrule.yaml`.

The SLI is computed by recording rules: Prometheus evaluates them every 30 seconds and stores the result as a new series, so alerts and panels read one cheap series instead of recomputing the ratio:

```bash
grep -n 'record: saga:slo_good_ratio:rate5m' -A1 charts/saga-observability/rules/saga.rules.yaml
```

You should see `sum(rate(saga_order_duration_seconds_bucket{le="5.0"}[5m])) / (sum(rate(saga_order_duration_seconds_count[5m])) > 0)`: orders within 5 s divided by all orders. The `> 0` makes a window without traffic record nothing instead of 0/0.

```bash
promq '{__name__=~"saga:slo_good_ratio:.*"}'
```

You should see four lines, `rate5m`, `rate30m`, `rate1h` and `rate6h`, each with a value of `1` or very close to it. Now the burn rate, as the SLO panel draws it on its right axis:

```bash
promq '(1 - saga:slo_good_ratio:rate1h) / 0.01'
```

You should see `{}` and `0` (or a tiny number): no budget is being spent. Prometheus keeps only 6 hours here, so the "30-day budget" is a convention for the thresholds, not something this cluster can measure.

The rules are unit-tested with `promtool` against made-up series ("20 % of orders slow for 10 minutes: does the fast burn fire?"):

```bash
make rules-test
```

You should see `Processing test file: saga.rules.test.yaml` and `SUCCESS`. It runs `promtool` from the same Prometheus image version the cluster runs (`PROMETHEUS_IMAGE` in `versions.env`); the first run pulls the image.

```bash
kubectl --context kind-order-saga get prometheusrule saga-observability -n observability
```

You should see the object with its age. The Prometheus Operator turns it into a rule file inside the Prometheus pod; `promq 'ALERTS{team="saga"}'` lists any saga alert that is pending or firing (normally none).

## 6. Alerts: rule, route, email

**What it is.** An alert travels three hops:

1. **Prometheus** evaluates the alert rule every 30 seconds. When the expression returns something, the alert becomes **pending**; if it keeps returning something for the `for:` duration, it becomes **firing** and is sent to Alertmanager.
2. **Alertmanager** groups, deduplicates and routes it. Our route: alerts labelled `team="saga"` go to the receiver `mailpit`; everything else (including the many built-in cluster alerts of kube-prometheus-stack) goes to `null`. It waits `group_wait: 30s` before the first email of a group and `group_interval: 5m` between updates.
3. **Mailpit** (the fake mail server from day 1) receives the email at `oncall@example.local`.

**Defined in** `charts/saga-observability/rules/saga.rules.yaml` (group `saga.alerts`) for the rule, and `alertmanager.config` in `deploy/local/values/kube-prometheus-stack.yaml` for the route.

The rule this section exercises:

```bash
grep -n -A8 'alert: PaymentErrorRateHigh' charts/saga-observability/rules/saga.rules.yaml
```

You should see the expression (failed payments divided by all payments over 5 minutes, `> 0.25`), `for: 5m`, the labels `severity: warning` and `team: saga`, and a summary. Payments decline 10 % on purpose, so the normal ratio is about 0.1.

The route, as Alertmanager loaded it. In the second terminal (stop `make prom` first, or use a third terminal):

```bash
make alerts
```

You should see `Forwarding from 127.0.0.1:9093 -> 9093`. Then:

```bash
curl -s localhost:9093/api/v2/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["config"]["original"])' | sed -n '/^route:/,/^inhibit_rules:/p'
```

You should see `receiver: "null"` at the top, then under `routes:` the receiver `mailpit` with `matchers: - team="saga"`, followed by `group_wait: 30s`, `group_interval: 5m` and `repeat_interval: 4h`. Stop `make alerts` with Ctrl-C.

Open Mailpit in the browser. In a free terminal:

```bash
kubectl --context kind-order-saga port-forward -n platform svc/mailpit 8025:8025
```

You should see `Forwarding from 127.0.0.1:8025 -> 8025`. `http://localhost:8025` shows the inbox: mostly order emails from notifications. Mailpit keeps the newest 25 000 messages (about three hours of loadgen traffic; `MP_MAX_MESSAGES` in `deploy/local/platform/mailpit.yaml`), so an alert email stays findable during a drill.

### Exercise: the payment-outage drill

Make every payment fail, watch the alert go pending, fire and arrive by email, then fix it and watch it resolve. It takes about 15 minutes; keep `make prom` running in one terminal and Mailpit's port-forward in another.

Break payments (Helm upgrades the release with one value changed and rolls the pod):

```bash
helm --kube-context kind-order-saga -n shop upgrade payments charts/service --reuse-values --set env.PAYMENT_FAILURE_RATE=1
```

You should see `Release "payments" has been upgraded. Happy Helming!`. Note the time. Every order is now cancelled with `payment_failed`, and inventory releases the stock each time (compensation, day 1). Watch the ratio climb:

```bash
promq 'sum(rate(saga_payments_total{result="failed"}[5m])) / sum(rate(saga_payments_total[5m]))'
```

You should see `{}` and a number that grows over the next five minutes from about 0.1 to `1`. Watch the alert:

```bash
promq 'ALERTS{alertname="PaymentErrorRateHigh"}'
```

You should see nothing for the first minute or so, then a line with `'alertstate': 'pending'` (the ratio crossed 0.25), and about five minutes later `'alertstate': 'firing'` (the `for: 5m` has passed). Thirty seconds after that (`group_wait`), the email arrives:

```bash
curl -s 'localhost:8025/api/v1/search?query=subject:PaymentErrorRateHigh' | python3 -c 'import json,sys; [print(m["Created"], m["Subject"]) for m in json.load(sys.stdin)["messages"]]'
```

You should see `<time in UTC> [FIRING:1] PaymentErrorRateHigh (observability/kps-prometheus warning saga)`, about 7 minutes after the upgrade. Open it in the Mailpit UI to see the summary with the failure percentage. Detection time is the 5-minute rate window plus the 5-minute `for:`, which is a deliberate trade: shorter means more false alarms from a few unlucky declines.

The SLO does not move during this drill: `promq 'saga:slo_good_ratio:rate5m'` stays at `1`. Cancelled orders are fast final states, so customers get a quick (bad) answer. A payment outage is a business problem that the latency SLO does not see, which is why it has its own alert.

**Restore.** Install payments again from its values file, exactly as `local-up.sh` does:

```bash
helm --kube-context kind-order-saga -n shop upgrade payments charts/service -f deploy/local/values/payments.yaml
```

You should see `Release "payments" has been upgraded.`. Confirm the rate is back:

```bash
helm --kube-context kind-order-saga -n shop get values payments | grep PAYMENT_FAILURE_RATE
```

You should see `PAYMENT_FAILURE_RATE: "0.1"`. The alert resolves once the 5-minute window has drained below 0.25 (about 5 minutes), and Alertmanager sends `[RESOLVED] PaymentErrorRateHigh ...` at its next `group_interval`, up to 5 minutes later. Run the search command above again until you see both emails.

## 7. Stuck PENDING: the alert for the missing outbox

**What it is.** ADR 0003 accepts a known gap: there is no transactional outbox. `orders` commits the PENDING order and then publishes `orders.created` as two separate steps. If the process dies between them, the order stays PENDING forever and no event will ever move it. Nothing in the saga notices. The gauge `saga.orders.stuck_pending` is the safety net: each `orders` pod periodically counts orders that have been PENDING for more than 2 minutes (a healthy order takes a quarter of a second), and `OrdersStuckPending` (critical) fires when that count stays above 0 for 2 minutes.

**Defined in** `internal/orders/metrics.go` (`RegisterMetrics`, the SQL `SELECT count(*) FROM orders WHERE status = 'PENDING' AND created_at < now() - interval '2 minutes'`, run on every metric export with a 2 s timeout) and the rule `OrdersStuckPending` in `charts/saga-observability/rules/saga.rules.yaml`. Both `orders` replicas query the same database and report the same number, so the rule uses `max`, not `sum`.

```bash
promq 'saga_orders_stuck_pending'
```

You should see two lines, one per `orders` pod, both ending in `0`.

### Exercise: take NATS away and watch the alert stay quiet

The obvious way to "lose" an event is to stop NATS. Try it, and keep the outage under two minutes. Scale NATS to zero:

```bash
kubectl --context kind-order-saga scale statefulset nats -n platform --replicas=0
```

You should see `statefulset.apps/nats scaled`. NATS takes a few seconds to shut down cleanly; wait until its pod is gone:

```bash
kubectl --context kind-order-saga wait --for=delete pod/nats-0 -n platform --timeout=60s
```

You should see `pod/nats-0 condition met`. (An order posted before this point is still accepted with `202`.) Place an order:

```bash
curl -s -w ' %{http_code}\n' -X POST localhost:8080/orders -H 'Content-Type: application/json' -d '{"customer_email":"tour@example.local","items":[{"sku":"SKU-001","qty":1}]}'
```

You should see `503`, with either `{"error":"temporarily unavailable"}` (orders itself refused) or `no healthy upstream` (about 30 seconds in, the readiness probes have taken both `orders` pods out of the Service and Envoy has nowhere to send the request). Either way, check the gauge:

```bash
promq 'max(saga_orders_stuck_pending)'
```

You should see `{}` and `0`. **Why the alert does not fire.** The POST failed loudly instead of leaving a PENDING order behind. Look at `create` in `internal/orders/api.go`: when the publish fails, orders deletes the row it just inserted (`DELETE FROM orders WHERE id = $1`) and answers 503, so the client knows to retry and there is nothing stuck. Once readiness fails, the request does not even reach orders. A stopped broker is the failure this design handles well; the gap in ADR 0003 is a *crash* between the commit and the publish, which a NATS outage does not produce. (One caveat: orders that were already published but not yet finished when NATS stopped wait in JetStream and stay PENDING until it is back. An outage longer than 2 minutes can therefore light the gauge for those few orders; they complete by themselves when NATS returns.)

**Restore.**

```bash
kubectl --context kind-order-saga scale statefulset nats -n platform --replicas=1 && kubectl --context kind-order-saga rollout status statefulset/nats -n platform --timeout=120s
```

You should see `partitioned roll out complete: 1 new pods have been updated...`. Within about 20 seconds the services are ready again; `make local-smoke` prints `SMOKE OK`.

### Exercise: simulate the crash, and watch the alert fire

To produce the real failure, write what a crash would leave behind: a PENDING order with no event, created three minutes ago. The id is made up and easy to recognise:

```bash
kubectl --context kind-order-saga exec -n platform postgres-0 -- psql -U postgres -d orders -c "INSERT INTO orders (id, customer_email, items, status, created_at) VALUES ('00000000-0000-0000-0000-00000000dead', 'tour@example.local', '[]', 'PENDING', now() - interval '3 minutes')"
```

You should see `INSERT 0 1`. Within 15 to 30 seconds (the next metric push, then the next scrape):

```bash
promq 'max(saga_orders_stuck_pending)'
```

You should see `{}` and `1`. Then `promq 'ALERTS{alertname="OrdersStuckPending"}'` shows `pending`, and two minutes later `firing`. Half a minute after that, Mailpit has `[FIRING:1] OrdersStuckPending (observability/kps-prometheus critical saga)`:

```bash
curl -s 'localhost:8025/api/v1/search?query=subject:OrdersStuckPending' | python3 -c 'import json,sys; [print(m["Created"], m["Subject"]) for m in json.load(sys.stdin)["messages"]]'
```

In real life the runbook would be: find the order (`SELECT id, created_at FROM orders WHERE status = 'PENDING' ORDER BY created_at`), then either re-publish its `orders.created` or cancel it. The outbox (ADR 0003, iteration 2) removes the cause.

**Restore.** Delete the fake order:

```bash
kubectl --context kind-order-saga exec -n platform postgres-0 -- psql -U postgres -d orders -c "DELETE FROM orders WHERE id = '00000000-0000-0000-0000-00000000dead'"
```

You should see `DELETE 1`. The gauge drops to 0 within 30 seconds, the alert resolves, and a `[RESOLVED] OrdersStuckPending` email follows within 5 minutes.

## 8. Resource cost

**What it is.** Observability is not free. The stack runs eleven pods next to the saga's six, and all of it must fit, with the platform and the services, in the 6 GB the local cluster is allowed. metrics-server, installed today, is what makes `kubectl top` work (it was the error at the end of the day-2 tour).

**Defined in** the `resources` blocks of every values file in `deploy/local/values/`; the planned limits and measurements are in ADR 0008, "Memory budget".

```bash
kubectl --context kind-order-saga top pods -n observability --sum
```

You should see one line per pod with its CPU and memory, and a total at the bottom of about 1.2 to 1.4 GB. The big ones: Grafana about 400 MiB (with its two sidecars), Prometheus about 300 MiB, Tempo about 100 to 250 MiB, Loki about 100 MiB, the Collector about 90 MiB. The rest are 10 to 35 MiB each.

```bash
kubectl --context kind-order-saga top nodes
```

You should see three nodes adding up to about 3.5 to 4 GB of memory. Compare with the saga itself:

```bash
kubectl --context kind-order-saga top pods -n shop --sum
```

You should see six pods of 10 to 20 MiB each, about 100 MiB in total: the observability stack uses more than ten times what it observes. That ratio is normal for a small system; on a large one it shrinks.

**What would be dropped first** if memory got tight, from first to last:

1. **node-exporter (x3) and kube-state-metrics** (about 75 MiB). They feed kube-prometheus-stack's built-in cluster dashboards and alerts, which all go to the `null` receiver here. No saga alert or panel uses them.
2. **Grafana** (about 400 MiB, the largest single pod). Every query it runs can also be typed into Prometheus' own UI (`make prom`) or `curl`ed from Tempo and Loki, as this tour did. You lose the comfort, not the data or the alerts.
3. **Loki** (about 100 MiB). Logs are still in `kubectl logs` for as long as the pods live; you lose the search across pods and the trace links.
4. **Tempo** (about 100 to 250 MiB, growing with the traces it holds). Without it there are no traces, the hardest signal to replace.
5. **Prometheus, Alertmanager and the Collector** stay to the end: without them nobody is told when the saga breaks.

Lower-risk knobs come before dropping anything: shorter retention (6 h today), fewer Grafana sidecars, or sampling traces in the Collector.

## 9. What changes on GKE

The point of the Collector in the middle is that this section is short. On GKE the services, the chart and `otel.endpoint` stay the same; only the Collector's exporters change (ADR 0008, "The swappable exporter").

| Here (kind) | On GKE |
|---|---|
| Traces: `otlp_grpc/tempo` exporter to Tempo | `googlecloud` exporter to Cloud Trace |
| Metrics: `prometheus` exporter scraped by Prometheus | `googlemanagedprometheus` exporter to Managed Service for Prometheus (Cloud Monitoring); the PromQL rules and dashboard can be kept |
| Logs: `otlp_http/loki` exporter to Loki | `googlecloud` exporter to Cloud Logging, which also collects container stdout by itself, so panics and third-party logs are no longer lost |
| No credentials (everything inside the cluster) | The Collector's Kubernetes service account is bound with Workload Identity to a Google service account with `roles/cloudtrace.agent`, `roles/monitoring.metricWriter` and `roles/logging.logWriter`; no keys |
| Resource labels from the downward API | Plus the `resourcedetection` processor (GCP detector), so every span and metric knows its project, cluster and location |
| Alertmanager to Mailpit | Cloud Monitoring alerting policies (or Alertmanager kept) to a real channel |
| `kubectl top` from metrics-server | Built in |
| Everything on `emptyDir`, 6 h | Managed backends with their own retention and pricing |

In the Collector values this would be a change like the following (a sketch, not applied):

```yaml
exporters:
  googlecloud: {}
  googlemanagedprometheus: {}
processors:
  resourcedetection:
    detectors: [gcp]
service:
  pipelines:
    traces:  {processors: [memory_limiter, resourcedetection, batch], exporters: [googlecloud]}
    metrics: {processors: [memory_limiter, resourcedetection, batch], exporters: [googlemanagedprometheus]}
    logs:    {processors: [memory_limiter, resourcedetection, batch], exporters: [googlecloud]}
```

During a migration the Collector can send to both, Tempo and Cloud Trace at once, and you compare before switching off the local stack.

## Before you leave

Stop every port-forward with Ctrl-C. Check that every exercise is undone:

```bash
kubectl --context kind-order-saga get deployment otel-collector -n observability; kubectl --context kind-order-saga get statefulset nats -n platform; helm --kube-context kind-order-saga -n shop get values payments | grep PAYMENT_FAILURE_RATE
```

You should see `otel-collector` `1/1`, `nats` `1/1` and `PAYMENT_FAILURE_RATE: "0.1"`.

```bash
make local-smoke && make obs-check
```

You should see `SMOKE OK`, then one `ok` line per query and `OBS OK`.

## Going further

[ADR 0008](adr/0008-observability-stack.md) explains every choice in this tour: the Collector in the middle, push over OTLP instead of scraping each service, logs over OTLP instead of reading container stdout, the open-source stack instead of a SaaS, the memory budget, and the GKE exporters. When you have finished this tour and the Day 3 checkpoint in the README, you should be able to defend it.
