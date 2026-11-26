# ADR 0009: Pod Security Admission and NetworkPolicies

## Status

Accepted

## Context

Day 4 hardens the cluster. Until now any pod could run as root, add capabilities, mount host paths or talk to the API server with its ServiceAccount token, and nothing stopped a manifest that did. The goal is that what runs is locked down, and that the cluster refuses anything that is not, using only what Kubernetes ships: Pod Security Admission (PSA) for what a pod may do, and NetworkPolicy for whom it may talk to. This ADR covers both; the Pod Security half comes first.

## Decision: Pod Security

### `restricted` where our workload lives, `baseline` elsewhere

Every namespace carries the three PSA labels (`enforce`, `warn`, `audit`), each pinned with `-version: v1.37`, so a cluster upgrade cannot silently change what a level means. The labels live in `deploy/local/platform/namespaces.yaml`.

| Namespace | enforce | warn / audit | What runs there |
|---|---|---|---|
| `shop` | `restricted` | `restricted` | the five services (our chart) |
| `platform` | `restricted` | `restricted` | Postgres, NATS, Mailpit (our manifests, our values) |
| `observability` | `baseline` | `restricted` | kube-prometheus-stack, Tempo, Loki, OTel Collector |
| `envoy-gateway-system` | `baseline` | `restricted` | Envoy Gateway controller and the proxy it creates |
| `argocd` | `baseline` | `restricted` | Argo CD (ADR 0010) |

- `restricted` forbids root, privilege escalation, any added capability (everything is dropped), host namespaces, host paths and unconfined seccomp. `shop` and `platform` hold the code and the data of the saga, and every pod spec in them is ours: the service chart, two plain manifests and the NATS chart's values. Making them pass costs a few lines each, so the strict level is enforced, not just audited.
- `baseline` forbids the known privilege escalations (privileged containers, host namespaces, host paths, added dangerous capabilities) but allows root and does not require seccomp or dropped capabilities. It is enforced where the pod specs come from third-party charts that we only configure. `warn` and `audit` stay at `restricted`, so every `helm upgrade` prints what would still fail and the API server's audit log records it: the gap is visible, not hidden.
- `kube-system` and `local-path-storage` keep the cluster defaults (no labels). They are kind's own components (kindnet and kube-proxy need host networking and capabilities) and are not ours to harden.

### What `shop` and `platform` run as

- The service chart (`charts/service`): a pod-level `securityContext` with `runAsNonRoot: true`, `runAsUser: 65532`, `runAsGroup: 65532` and `seccompProfile: RuntimeDefault`, on top of the container-level one it already had (read-only root filesystem, no privilege escalation, all capabilities dropped). 65532 is the image's numeric `USER`, so the pod-level value only states what the image already does, and the kubelet can verify `runAsNonRoot` without reading the image.
- Postgres runs as uid and gid `70`, the `postgres` user of the alpine image, with `fsGroup: 70`. Its root filesystem is read-only; the socket directory `/var/run/postgresql` and `/tmp` are `emptyDir`s, and the data directory is the PVC. The image's entrypoint used to start as root and switch to this user, so an existing data directory already belongs to it; on a fresh volume `initdb` runs as uid 70 directly. Kind's local-path volumes are host directories, where the kubelet does not apply `fsGroup`; they are created world-writable, which is what lets a non-root process initialise them.
- Mailpit runs as uid and gid `65534` (`nobody`) with a read-only root filesystem; its database is on the existing `emptyDir`, and `/tmp` is another one.
- NATS (chart values only) runs as uid and gid `1000` with `fsGroup: 1000`, and so do its config reloader and its Prometheus exporter sidecars. All three have a read-only root filesystem; the chart already gives the PID file directory and the config their own volumes, and JetStream writes to its PVC. A JetStream store written by the old root-run server is not writable by uid 1000 (`storage directory is not writable`), so the switch needed a fresh volume (`make local-down && make local-up`); the local cluster is disposable, a real migration would `chown` the volume once.
- All of them drop `ALL` capabilities, set `allowPrivilegeEscalation: false` and use the `RuntimeDefault` seccomp profile. Nothing loosens the policy: where a process needs a writable path it gets an `emptyDir`.

### Why the observability charts are not pushed to `restricted` yet

kube-prometheus-stack, Tempo, Loki and the OTel Collector are separate charts with a dozen containers, init containers and sidecars between them, each with its own `securityContext` knobs and defaults, and those defaults change between chart versions. With node-exporter gone, most of them already pass `restricted`: at this commit only the OTel Collector (no `runAsNonRoot`, no seccomp, capabilities and privilege escalation not dropped) and Tempo (seccomp, capabilities, privilege escalation) do not, and `warn` names them on every install. Enforcing `restricted` there would mean that a chart upgrade adding a non-compliant container fails at admission halfway through an install of the stack we debug everything else with, for pods that hold no business data and are not reachable from outside the cluster. Closing the two gaps through values and then raising the level is left for a later iteration; `baseline` still blocks the escalations that matter.

`envoy-gateway-system` passes `restricted` today as well, but its proxy Deployment is generated by the Envoy Gateway controller, not by a manifest we review, so it stays at `baseline` enforce for the same reason. `argocd` is at `baseline` for the same reason too: with the pinned chart and our values the Argo CD install prints no `restricted` warning (ADR 0010), but its pod specs come from a third-party chart, so raising it waits until that has held across a chart upgrade.

### node-exporter is disabled

node-exporter needs `hostNetwork`, `hostPID` and `hostPath` mounts of `/proc`, `/sys` and `/`; it fails `baseline`, not just `restricted`. On kind it would only measure the Docker host (every "node" is a container on the same machine), and no saga panel or alert uses its metrics. It is turned off (`nodeExporter.enabled: false`) rather than exempting a namespace for it. metrics-server, in `kube-system`, still serves `kubectl top`, which is how the memory budget is checked; kube-prometheus-stack's built-in node dashboards and node alerts go empty. On GKE, node metrics come from Cloud Monitoring's system metrics, or node-exporter runs in its own namespace labelled `privileged`, which is a decision to make there, explicitly.

### No ServiceAccount tokens

- Each service has its own ServiceAccount, with no Role or RoleBinding: none of them calls the Kubernetes API.
- `automountServiceAccountToken: false` is set on the ServiceAccount and on the pod spec. Either alone would do today; both means a later change to one does not hand the token back. A compromised service therefore has no credential for the API server at all, rather than a token whose permissions merely happen to be empty (the `system:serviceaccounts` group can still read discovery endpoints, and RBAC added later for something else could apply to it).
- Postgres, Mailpit and NATS run under their namespace's `default` ServiceAccount, with `automountServiceAccountToken: false` on the pod spec (the manifests, and `podTemplate` in the NATS values): none of them, nor the NATS config reloader or exporter sidecars, calls the Kubernetes API.

## Decision: NetworkPolicies

### Enforcement on kind

kind's default CNI, kindnet, enforces NetworkPolicy through the embedded kube-network-policies controller (kind v0.33.0, `kindest/kindnetd:v20260820-69b56db7`): an nftables table `kindnet-network-policies` on each node hands packets to or from pods selected by a policy to a userspace queue, which evaluates the policies. Verified before relying on it: a pod in a throwaway namespace reached `orders` (HTTP 200); with a deny-all policy in that namespace, the same request from a new pod timed out, DNS included (`Resolving timed out after 3003 milliseconds`), and it worked again once the policy was deleted.

One caveat found on the way: a pod is added to the policy sets a moment after it starts, and the queue rule is `queue flags bypass`, so the very first packets of a new pod pass unfiltered. A probe that ran `curl` at container start still got through the deny-all; one that waited five seconds was blocked. `make policy-test` therefore waits for its probe pods to be Ready, then five more seconds. For the services this is a window of about a second at startup, before they have opened any connection.

### The traffic matrix

`shop` and `platform` deny all ingress and all egress (`default-deny`), then allow exactly these flows. Each allow is an ingress rule on the receiver and, where the sender's namespace denies egress, an egress rule on the sender. Peers are always a `namespaceSelector` on `kubernetes.io/metadata.name` together with a `podSelector` on the pod's labels, never an IP.

| From | To | Port | Policies |
|---|---|---|---|
| Envoy proxy (`envoy-gateway-system`, `app.kubernetes.io/name: envoy`, `component: proxy`) | `orders` | 8080 | `shop/orders-ingress` |
| `loadgen` | `orders` | 8080 | `shop/loadgen-egress-orders`, `shop/orders-ingress` |
| `orders`, `inventory`, `payments` | `postgres` (`platform`) | 5432 | `shop/egress-postgres`, `platform/postgres-ingress` |
| `orders`, `inventory`, `payments`, `notifications` | `nats` (`platform`) | 4222 | `shop/egress-nats`, `platform/nats-ingress` |
| `notifications` | `mailpit` (`platform`) | 1025 | `shop/notifications-egress-mailpit`, `platform/mailpit-ingress` |
| all five services | `otel-collector` (`observability`) | 4318 | `shop/egress-otel-collector`, `observability/otel-collector-ingress` |
| Prometheus (`observability`) | NATS exporter sidecar (`platform`) | 7777 | `platform/nats-ingress` |
| Alertmanager (`observability`) | `mailpit` (`platform`) | 1025 | `platform/mailpit-ingress` |
| every pod in `shop` / `platform` | kube-dns (`kube-system`, `k8s-app: kube-dns`) | 53 TCP+UDP | `*/allow-dns-egress` |
| any pod in `observability` / `argocd` | same namespace | any | `*/allow-same-namespace` |
| Prometheus | `otel-collector` | 8889 | `observability/otel-collector-ingress` |
| API server (host network) | Prometheus Operator | 10250 | `observability/prometheus-operator-webhook-ingress` |

Only `orders` accepts connections from outside its namespace's workloads, and only from the Gateway's proxy (and from loadgen). Nothing in `shop` or `platform` reaches the internet or any address not listed. The policies live in `deploy/local/policies/`, one file per namespace; Argo CD applies them through the `policies` Application (ADR 0010), so a change to a policy is a commit, and a policy deleted or edited by hand is restored within seconds.

The last row is not in the original matrix; it was found while verifying it. kube-prometheus-stack registers admission webhooks (PrometheusRule and AlertmanagerConfig validation, PrometheusRule mutation) served by the operator pod on port 10250. The API server runs on the control-plane node's host network, so no pod or namespace selector matches it, and the source could only be named with an `ipBlock` of the node network. Without the allow, every PrometheusRule apply waited 10 s per webhook and the webhooks failed open (`failurePolicy: Ignore`), so rules were installed unvalidated. The allow therefore names no source and is narrowed to that pod and that port; the endpoint is TLS and serves only the webhooks and the operator's metrics. With it, an invalid rule is rejected again at admission (`could not parse expression`).

### Why `observability` and `argocd` keep egress open

Their egress goes to the API server, to every kubelet (`/metrics`, `/metrics/cadvisor`), and to CoreDNS: Prometheus scrapes node IPs, Grafana's sidecars and the operator watch the API server, and Argo CD pulls from GitHub. On kind none of these can be named without IP blocks of the node network and of GitHub's ranges, which change. These namespaces hold no business data and are reached by nothing outside the cluster, so they get default-deny ingress (with traffic inside the namespace allowed) and open egress. Their outbound traffic into `shop` and `platform` is still limited by those namespaces' ingress policies: Prometheus can scrape the NATS exporter and nothing else there.

### What bypasses the policies on kindnet

Checked on this cluster rather than assumed:

- **kubelet probes are not filtered.** The node table accepts locally generated traffic from root processes (`meta skuid 0 accept`) before any policy is evaluated, and the kubelet runs as root on the pod's own node. Every pod in `shop` and `platform` stayed Ready with no restarts under default-deny, including after a rollout restart of all of them. From the pod's own node, root `curl` reached `orders:8080` and Mailpit's UI on 8025 (neither allowed by any rule for that source); the same request as uid 65534, or from another node, timed out.
- **`kubectl port-forward` is not filtered.** The container runtime opens the forwarded connection inside the pod's network namespace, so it never crosses the node's policy hooks. `make local-smoke` (Mailpit 8025), `make obs-check` (Prometheus 9090) and `scripts/trace-check.sh` (Tempo 3200, Loki 3100) all pass, and none of those ports is open to other pods.
- **Traffic from other nodes is filtered.** Host-network traffic that arrives from another node (the API server calling a webhook, for example) is treated as an unknown source and denied, hence the operator allow above.

On GKE with Dataplane V2 (Cilium) these behaviours differ (host traffic is an identity of its own, probes and port-forwards take other paths, and the control plane sits outside the cluster network), so the policies, the probes and the webhook allow must be re-verified there, with `make policy-test` and the checks above, before they are trusted.

### How it is tested

`make policy-test` starts short-lived probe pods from `$CURL_IMAGE` (pinned in `deploy/local/versions.env`), built to pass `restricted` (uid 100, all capabilities dropped, no privilege escalation, `RuntimeDefault` seccomp, no ServiceAccount token). Each impersonates a sender through its `app.kubernetes.io/name` label, or has none, and runs `curl --max-time 3`. Two flows must connect (`loadgen` → `orders:8080`, `notifications` → `mailpit:1025`); five must time out before connecting (an unlabelled pod → `orders` and → `postgres`, `loadgen` → `postgres`, a `platform` pod → `orders`, `notifications` → `https://example.com`). It prints `POLICY OK` only if all seven match, and deletes the probes.

## Consequences

- A manifest that asks for root, a capability, a host path or privileged mode in `shop` or `platform` is rejected at admission (`make policy-test` checks this with server-side dry-runs), not discovered later.
- Changing a namespace's level is checked with a server-side dry-run relabel, which reports existing pods that would violate it. PSA evaluates pods only when a label value changes, so `make policy-test` dry-runs `enforce-version=latest` together with the namespace's own level and expects no warning; it also runs the same check on `observability` at `restricted` and requires warnings, so the check is proven able to fail.
- NATS switching user needed a from-zero rebuild of the local cluster once.
- Node-level metrics are gone from the local cluster.
- The observability, gateway and Argo CD namespaces are only at `baseline`; the warnings on every install are the to-do list.
- Every new flow needs a policy change on both ends; a missing allow shows up as a timeout (a stalled saga, a target down in Prometheus), not as an error that names the policy.
- Policies are only as good as the CNI that enforces them: on kindnet a pod's first moment is unfiltered, and node-local root traffic is always allowed.
- `observability` and `argocd` can still open connections anywhere; only what they receive is restricted.
- On GKE everything in the NetworkPolicy section must be re-verified on Dataplane V2.
