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
| `argocd` | `baseline` | `restricted` | Argo CD (installed in a later task) |

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

`envoy-gateway-system` passes `restricted` today as well, but its proxy Deployment is generated by the Envoy Gateway controller, not by a manifest we review, so it stays at `baseline` enforce for the same reason; `argocd` starts at `baseline` because the Argo CD chart is not installed yet and its components have not been checked.

### node-exporter is disabled

node-exporter needs `hostNetwork`, `hostPID` and `hostPath` mounts of `/proc`, `/sys` and `/`; it fails `baseline`, not just `restricted`. On kind it would only measure the Docker host (every "node" is a container on the same machine), and no saga panel or alert uses its metrics. It is turned off (`nodeExporter.enabled: false`) rather than exempting a namespace for it. metrics-server, in `kube-system`, still serves `kubectl top`, which is how the memory budget is checked; kube-prometheus-stack's built-in node dashboards and node alerts go empty. On GKE, node metrics come from Cloud Monitoring's system metrics, or node-exporter runs in its own namespace labelled `privileged`, which is a decision to make there, explicitly.

### No ServiceAccount tokens

- Each service has its own ServiceAccount, with no Role or RoleBinding: none of them calls the Kubernetes API.
- `automountServiceAccountToken: false` is set on the ServiceAccount and on the pod spec. Either alone would do today; both means a later change to one does not hand the token back. A compromised service therefore has no credential for the API server at all, rather than a token whose permissions merely happen to be empty (the `system:serviceaccounts` group can still read discovery endpoints, and RBAC added later for something else could apply to it).
- Postgres, Mailpit and NATS run under their namespace's `default` ServiceAccount. Tokens there are left to a later iteration; NetworkPolicy (below) is what limits what they can reach.

## Consequences

- A manifest that asks for root, a capability, a host path or privileged mode in `shop` or `platform` is rejected at admission (`make policy-test` checks this with server-side dry-runs), not discovered later.
- Changing a namespace's level is checked the same way: `kubectl label --dry-run=server` reports existing pods that would violate it, and `make policy-test` expects no such warning for any namespace at its own level.
- NATS switching user needed a from-zero rebuild of the local cluster once.
- Node-level metrics are gone from the local cluster.
- The observability, gateway and Argo CD namespaces are only at `baseline`; the warnings on every install are the to-do list.
