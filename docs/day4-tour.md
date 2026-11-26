# Day 4 tour: locking it down and deploying from Git

Day 3 made the saga observable. Day 4 changed two things about how it runs. First, the cluster now refuses what it should not run and blocks traffic nobody asked for: Pod Security levels, ServiceAccounts without tokens, and default-deny NetworkPolicies. Second, the workload is no longer installed from your laptop: Argo CD, running inside the cluster, deploys whatever Git says, and a GitHub Actions workflow builds, scans and publishes the images and commits the new tag. You did not write this code. This tour is how you come to own it: for each piece you see what it is, which file defines it, how to look at it live, and (where it is safe) how to break it and watch.

You need the cluster running (`make local-up`, then `make local-smoke` prints `SMOKE OK` and `make policy-test` prints `POLICY OK`). As before, every command passes `--context kind-order-saga` (or `--kube-context` for Helm), the commands are written for zsh and bash, and you run them from the repository root. Some sections also need `gh` (the GitHub CLI, logged in) and Docker.

**Which branch the cluster follows.** Argo CD tracks one Git branch, set by `ARGOCD_REVISION` when `make local-up` runs (default `main`). While the day-4 pull request is open, the cluster was set up with `ARGOCD_REVISION=day4/gitops-security make local-up`. After the pull request is merged, run `make local-up` once without the variable: it switches Argo CD to `main`. Careful before the merge: every `make local-up` re-applies the setting, so running it without `ARGOCD_REVISION` on a cluster that tracks a branch moves it back to `main`, which has no `deploy/argocd/` yet, so the `root` Application fails until you run it again with the variable. Check which one yours follows:

```bash
kubectl --context kind-order-saga get application root -n argocd -o jsonpath='{.spec.source.targetRevision}{"\n"}'
```

You should see `day4/gitops-security` before the merge, `main` after it. Section 6 pushes to this branch.

**Argo CD reverts manual changes.** The five services in `shop`, the dashboard and rules (`saga-observability`) and every NetworkPolicy now belong to Argo CD. A `kubectl` change to one of them is undone within seconds; that is the point of section 4. The platform (Postgres, NATS, Mailpit, the observability stack, Envoy Gateway, Argo CD itself) is still installed by `scripts/local-up.sh` with Helm, so the day-2 and day-3 exercises on NATS, Postgres and the Collector still work as written. The day-3 payment-outage drill does not: `payments` is no longer a Helm release (see the note at the top of the day-3 tour).

Some commands run forever (port-forwards, `-w` watches). Run those in a **second terminal** and stop them with Ctrl-C. The local ports used are 9090 (Prometheus), 8025 (Mailpit) and 8443 (Argo CD).

## A few words first

- **Admission** is the step where the API server decides whether to accept an object before storing it. **Pod Security Admission** (PSA) is a built-in admission check that compares every pod against one of three levels: `privileged` (anything goes), `baseline` (blocks the known ways to escape to the node) and `restricted` (also requires the pod to run as a non-root user with every Linux capability dropped). A namespace label picks the level, and three modes act on it: `enforce` rejects, `warn` prints a warning to whoever applied the object, `audit` writes to the API server's audit log.
- A **ServiceAccount** is a pod's identity towards the Kubernetes API. By default Kubernetes mounts a token for it into every container, whether the program needs one or not.
- A **NetworkPolicy** says which pods may talk to which, by labels. Kubernetes only stores it; the network plugin (the **CNI**, here kind's kindnet) enforces it. As soon as one policy selects a pod for a direction (ingress or egress), everything in that direction that no policy allows is dropped. **Default-deny** means a policy that selects every pod and allows nothing, so each allowed flow must then be written down.
- **GitOps** means Git is the description of what runs, and an agent in the cluster makes the cluster match it. **Push** deploys have CI run `helm upgrade` against the cluster; **pull** deploys have the agent in the cluster fetch from Git. Argo CD is the pull agent here.

## 1. Pod Security: what a pod may do

**What it is.** Every namespace we own carries six labels: an `enforce`, `warn` and `audit` level, each pinned to the Kubernetes version whose definition of the level applies (`v1.37`), so a cluster upgrade cannot quietly change what `restricted` means.

| Namespace | enforce | warn / audit | Why |
|---|---|---|---|
| `shop`, `platform` | `restricted` | `restricted` | Our code and our data. Every pod spec there is ours (the service chart, two manifests, the NATS chart's values), so it was cheap to make them pass. |
| `observability`, `envoy-gateway-system`, `argocd` | `baseline` | `restricted` | Pod specs come from third-party charts. `baseline` still blocks the escapes; `warn` at `restricted` prints what would not pass on every install. |

**`restricted` forbids**, on top of `baseline`: running as root (`runAsNonRoot: true` is required), privilege escalation (`allowPrivilegeEscalation: false`), any Linux capability (`capabilities.drop: [ALL]`), and a missing seccomp profile (`RuntimeDefault` is required). **`baseline` forbids** privileged containers, the host's namespaces (network, PID, IPC), host paths and dangerous capabilities.

**Defined in** `deploy/local/platform/namespaces.yaml` (the labels), `charts/service/templates/deployment.yaml` (the pod and container `securityContext` of the services), `deploy/local/platform/postgres.yaml` and `mailpit.yaml`, and `podTemplate` in `deploy/local/values/nats.yaml`. [ADR 0009](adr/0009-network-policies-and-pod-security.md) explains each choice.

```bash
kubectl --context kind-order-saga get ns -L pod-security.kubernetes.io/enforce,pod-security.kubernetes.io/warn
```

You should see `shop` and `platform` with `restricted` in both columns, `argocd`, `envoy-gateway-system` and `observability` with `baseline` and `restricted`, and the rest (`default`, `kube-system`, ...) empty: kind's own components need host access and are not ours to harden.

Ask the API server to admit a privileged pod in `shop`. `--dry-run=server` runs every admission check and then stores nothing, so this is safe:

```bash
kubectl --context kind-order-saga -n shop run pst --image=busybox:1.37 --restart=Never --dry-run=server --overrides='{"spec":{"containers":[{"name":"pst","image":"busybox:1.37","securityContext":{"privileged":true}}]}}' -- true
```

You should see `Error from server (Forbidden): pods "pst" is forbidden: violates PodSecurity "restricted:v1.37": privileged (...), allowPrivilegeEscalation != false (...), unrestricted capabilities (...), runAsNonRoot != true (...), seccompProfile (...)`. The message lists every rule the pod breaks, each with the field that would fix it. Now an ordinary pod with no `securityContext` at all, the way most examples on the internet are written:

```bash
kubectl --context kind-order-saga -n shop run pst --image=busybox:1.37 --restart=Never --dry-run=server -- true
```

You should see the same `Forbidden` without the `privileged` item: in `shop`, a pod has to state that it is harmless. The same pod in a `baseline` namespace:

```bash
kubectl --context kind-order-saga -n observability run pst --image=busybox:1.37 --restart=Never --dry-run=server -- true
```

You should see `Warning: would violate PodSecurity "restricted:v1.37": ...` and then `pod/pst created (server dry run)`: admitted, with a warning from the `warn` mode. Run the first (privileged) command with `-n observability` and it is rejected too, as `violates PodSecurity "baseline:v1.37": privileged`.

What our own pods declare:

```bash
kubectl --context kind-order-saga get deployment orders -n shop -o jsonpath='{.spec.template.spec.securityContext}{"\n"}{.spec.template.spec.containers[0].securityContext}{"\n"}'
```

You should see two lines: the pod level `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` and the container level with `allowPrivilegeEscalation:false`, `capabilities: {drop: [ALL]}`, `readOnlyRootFilesystem:true`, `runAsNonRoot:true` and the seccomp profile. 65532 is the `nonroot` user of the distroless base image.

`make policy-test` checks all of this on every run: each namespace's level and version, a privileged pod rejected in `shop` and `platform`, and `kubectl label --dry-run=server` per namespace at its own level, which reports any running pod that would violate it. Section 3 runs it.

## 2. ServiceAccounts without tokens

**What it is.** Each service runs as its own ServiceAccount with no Role bound to it, and with `automountServiceAccountToken: false` on both the ServiceAccount and the pod. No token file exists inside the container. If someone takes over a service through a bug, there is no credential for the Kubernetes API to steal, not even one with empty permissions. Postgres, NATS and Mailpit run under their namespace's `default` ServiceAccount, also with the token not mounted.

**Defined in** `charts/service/templates/serviceaccount.yaml` and `deployment.yaml`; the platform manifests and `podTemplate` in `deploy/local/values/nats.yaml`.

```bash
kubectl --context kind-order-saga get pods -n shop -o custom-columns='POD:.metadata.name,SA:.spec.serviceAccountName,AUTOMOUNT:.spec.automountServiceAccountToken,MOUNTS:.spec.containers[0].volumeMounts[*].mountPath'
```

You should see six pods, each with its own ServiceAccount (`orders`, `inventory`, ...), `AUTOMOUNT` `false` and `MOUNTS` `<none>`: nothing at all is mounted into the container.

Look from the inside. The service images are distroless and have no shell or `ls`, so use Postgres, which runs under the same rule:

```bash
kubectl --context kind-order-saga exec -n platform postgres-0 -- ls /var/run/secrets/kubernetes.io
```

You should see `ls: /var/run/secrets/kubernetes.io: No such file or directory` and `command terminated with exit code 1`. Compare a pod that does need the API, Argo CD's server:

```bash
kubectl --context kind-order-saga exec -n argocd deploy/argocd-server -- ls /var/run/secrets/kubernetes.io/serviceaccount
```

You should see `ca.crt`, `namespace` and `token`. (Do not `cat` the token: it is a live credential.)

**Why it matters.** A token is worth what RBAC grants it. Ask the API server what each identity may do, without any token, by impersonating it:

```bash
kubectl --context kind-order-saga auth can-i list secrets -n shop --as=system:serviceaccount:shop:orders; kubectl --context kind-order-saga auth can-i list secrets -n shop --as=system:serviceaccount:argocd:argocd-application-controller
```

You should see `no`, then `yes`. `orders` could do nothing useful with a token today, but RBAC added later for something else could change that; with no token there is nothing to change. Argo CD's controller can read every Secret in `shop`, because it deploys there: it is the most powerful identity in this cluster, which is one reason its namespace has its own NetworkPolicies and its UI is never exposed.

## 3. NetworkPolicies: who may talk to whom

**What it is.** `shop` and `platform` deny all traffic in and out, then allow exactly the flows the saga needs. `observability` and `argocd` deny incoming traffic from other namespaces but keep outgoing traffic open (they reach the API server, the kubelets on node IPs and GitHub, none of which a pod selector can name).

```
                     envoy-gateway-system                     kube-system
                       Envoy proxy                              kube-dns :53  ◄── every pod in shop and platform
                           │ :8080
                           ▼
 shop    loadgen ──:8080──► orders     inventory   payments   notifications
                             │  │          │  │       │  │       │  │   │
                     :5432 ──┘  └─:4222────┘  └───────┘  └───────┘  │   └──:1025──┐
                       │  (orders, inventory,  (orders, inventory,  │             │
                       │   payments)            payments, notif.)   │             │
                       ▼                         ▼                  │             ▼
 platform           postgres                    nats :4222          │          mailpit :1025 ◄── Alertmanager
                                                nats exporter :7777 ◄── Prometheus
                                                                    │ :4318 (all five services)
                                                                    ▼
 observability                                               otel-collector :4318, :8889 ◄── Prometheus
```

Everything not drawn is dropped: `loadgen` cannot reach Postgres, nothing in `platform` can call `orders`, and no pod in `shop` or `platform` can reach the internet. Every peer is named by namespace plus pod labels, never by IP.

**Defined in** `deploy/local/policies/`, one file per namespace (`shop.yaml`, `platform.yaml`, `observability.yaml`, `argocd.yaml`). Argo CD applies them (the `policies` Application, section 4). ADR 0009 has the full matrix with the policy behind each row.

```bash
kubectl --context kind-order-saga get networkpolicy -A
```

You should see 19 policies: 8 in `shop`, 5 in `platform`, 4 in `observability`, 2 in `argocd`. Read one allow from both ends. The sender's side:

```bash
sed -n '/name: egress-nats/,/^---/p' deploy/local/policies/shop.yaml
```

You should see a policy that selects the pods `orders`, `inventory`, `payments` and `notifications` (`podSelector` with `matchExpressions ... In`), has `policyTypes: [Egress]`, and allows traffic `to` pods labelled `app.kubernetes.io/name: nats` in the namespace named `platform`, on TCP 4222. The receiver's side is `nats-ingress` in `deploy/local/policies/platform.yaml`: the same four senders from `shop` on 4222, plus Prometheus on 7777. A flow needs both: an egress allow where the sender's namespace denies egress, and an ingress allow where the receiver's denies ingress.

**Why DNS needs its own allow.** `shop` denies all egress, and looking up `nats.platform.svc.cluster.local` is itself a packet to kube-dns in `kube-system`. Without `allow-dns-egress` every connection fails before it starts, as `Resolving timed out`, and the error never mentions a policy.

Now prove the matrix with real traffic:

```bash
make policy-test
```

You should see the Pod Security checks from section 1 (`shop: restricted (v1.37)` ... `shop: rejected` ... `shop: no violations at restricted` ...), then seven probe lines starting with `ok`, two `allow` and five `deny`, then `POLICY OK`. It takes about 30 seconds. Each probe is a short-lived curl pod that pretends to be a sender by carrying its label (or no label at all) and tries one connection: `loadgen` → `orders:8080` and `notifications` → `mailpit:1025` must connect; an unlabelled pod → `orders` and → `postgres`, `loadgen` → `postgres`, a `platform` pod → `orders` and `notifications` → `https://example.com` must time out before connecting.

**What the policies do not see on kind** (measured, ADR 0009):

- The kubelet's liveness and readiness probes are not filtered: they come from the node itself as root, which kindnet lets through. That is why no pod went unready under default-deny.
- `kubectl port-forward` is not filtered either: the tunnel opens inside the pod's network namespace. `make prom`, `make grafana` and the Mailpit port-forward keep working although no policy allows them.
- A new pod's first second or so is unfiltered: kindnet adds it to its policy sets about one second after it starts. `make policy-test` waits five seconds after its probes are Ready for this reason.
- On GKE (Dataplane V2) all of this behaves differently and must be re-verified.

### Break it and watch: take one allow away

Take `payments` out of the NATS allow. Without NATS, payments never hears about reserved stock, so orders stay PENDING, and the `OrdersStuckPending` alert from day 3 fires. JetStream keeps the events, so nothing is lost: once the allow is back, payments works through them.

You will need Prometheus and Mailpit. In the second terminal:

```bash
make prom
```

You should see `Forwarding from 127.0.0.1:9090 -> 9090`. In a third:

```bash
kubectl --context kind-order-saga port-forward -n platform svc/mailpit 8025:8025
```

You should see `Forwarding from 127.0.0.1:8025 -> 8025`. Back in the first terminal, define the `promq` helper from the day-3 tour (one PromQL query, one line per result):

```bash
promq() { curl -s localhost:9090/api/v1/query --data-urlencode "query=$1" | python3 -c 'import json,sys; [print(r["metric"], r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'; }
```

You should see nothing; the function now exists in this shell.

**First try: Argo CD puts it back.** Delete a whole policy:

```bash
kubectl --context kind-order-saga delete networkpolicy notifications-egress-mailpit -n shop; sleep 5; kubectl --context kind-order-saga get networkpolicy notifications-egress-mailpit -n shop
```

You should see `networkpolicy.networking.k8s.io "notifications-egress-mailpit" deleted`, then the policy again with an `AGE` of a few seconds. Argo CD noticed that a policy Git describes was missing and created it again (self-heal, section 4). To a person with `kubectl` access the policies are read-only; the way to change them is a commit.

**Second try: pause Argo CD first.** To watch what a missing allow does, turn off automatic sync on the `policies` Application. The `root` Application manages the `policies` Application (app-of-apps, section 4) and would put its sync policy back, so pause `root` first:

```bash
kubectl --context kind-order-saga patch application root -n argocd --type json -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]' && kubectl --context kind-order-saga patch application policies -n argocd --type json -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]'
```

You should see `application.argoproj.io/root patched` and `application.argoproj.io/policies patched`. Now remove `payments` from the senders of `egress-nats`:

```bash
kubectl --context kind-order-saga patch networkpolicy egress-nats -n shop --type json -p '[{"op":"replace","path":"/spec/podSelector/matchExpressions/0/values","value":["orders","inventory","notifications"]}]'
```

You should see `networkpolicy.networking.k8s.io/egress-nats patched`. This time it stays: `kubectl --context kind-order-saga get application policies -n argocd` shows `OutOfSync`, and nothing acts on it. And yet payments keeps working:

```bash
kubectl --context kind-order-saga logs -n shop deploy/payments --since=20s | grep -c 'payment succeeded'
```

You should see a number around 30. **A policy decides whether a connection may start**; payments' connection to NATS was opened long ago and stays up. Make payments open a new one, by deleting its pod (the Deployment creates a replacement at once), and note the time:

```bash
kubectl --context kind-order-saga delete pod -n shop -l app.kubernetes.io/name=payments; date -u +%T
```

You should see `pod "payments-..." deleted` and the time in UTC. The new pod cannot reach NATS. Its client retries quietly, so it logs nothing; the kubelet shows the symptom:

```bash
kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=payments; kubectl --context kind-order-saga get events -n shop --field-selector reason=Unhealthy | tail -1
```

You should see one `payments-...` pod at `0/1`, and an event `Startup probe failed: Get "http://10.244.x.x:8080/healthz": dial tcp ...: connect: connection refused`: the service opens its HTTP port only once NATS and Postgres answer. The probe itself is not blocked by the policy (kubelet traffic bypasses it on kind); the service is simply not up. Two minutes after the start the kubelet restarts it, and keeps doing so with growing back-off. Meanwhile the events for payments pile up in JetStream:

```bash
promq 'max(nats_consumer_num_pending{consumer_name="payments"})'
```

You should see `{}` and a number that grows by about 110 a minute (loadgen's orders, minus the 10 % for out-of-stock SKUs that never reach payments). Orders are accepted (`202`) and stay PENDING. About two minutes after the pod was deleted, the stuck-PENDING gauge from day 3 rises:

```bash
promq 'max(saga_orders_stuck_pending)'
```

You should see `{}` and a number that grows every 15 seconds. `OrdersStuckPending` goes `pending` at once and fires two minutes later (its `for: 2m`):

```bash
promq 'ALERTS{alertname="OrdersStuckPending"}'
```

You should see a line with `'alertstate': 'pending'`, then `'alertstate': 'firing'` (measured: firing 4.5 minutes after the pod was deleted). Half a minute later the email arrives; Alertmanager → Mailpit is a separate allow, untouched:

```bash
curl -s 'localhost:8025/api/v1/search?query=subject:OrdersStuckPending' | python3 -c 'import json,sys; [print(m["Created"], m["Subject"]) for m in json.load(sys.stdin)["messages"]]'
```

You should see `<time> [FIRING:1] OrdersStuckPending (observability/kps-prometheus critical saga)` (and older ones, if you did the day-3 exercise). Note what the alert does not say: nothing about a NetworkPolicy. A missing allow shows up as a timeout and a stalled step; the policy files and `make policy-test` are where you look.

**Restore.** Turn automatic sync back on for `root`. It re-applies the `policies` Application as Git describes it, automatic sync included, and `policies` then puts `egress-nats` back:

```bash
kubectl --context kind-order-saga patch application root -n argocd --type merge -p '{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'
```

You should see `application.argoproj.io/root patched`. A few seconds later:

```bash
kubectl --context kind-order-saga get networkpolicy egress-nats -n shop -o jsonpath='{.spec.podSelector.matchExpressions[0].values}{"\n"}'; kubectl --context kind-order-saga get application root policies -n argocd
```

You should see `["orders","inventory","payments","notifications"]` and both Applications `Synced` and `Healthy` (measured: 2 s). If the payments container is running at that moment, its NATS client gets through on the next retry and the pod turns `1/1` within seconds. If it is waiting out a restart back-off (up to five minutes), skip the wait by deleting it once more:

```bash
kubectl --context kind-order-saga delete pod -n shop -l app.kubernetes.io/name=payments && kubectl --context kind-order-saga wait --for=condition=Ready pod -l app.kubernetes.io/name=payments -n shop --timeout=120s
```

You should see `pod "payments-..." deleted`, then `pod/payments-... condition met` about 10 seconds later. No order is lost: the events waited in JetStream, and every stuck order reaches a final state. Recovery is slower than you might expect, though (measured: about 10 minutes after the restore for a 5-minute outage). Payments takes a few hundred waiting events at once and handles them one by one, 200 ms each; any it has held longer than the 30-second ack deadline is delivered again, and a redelivery still sleeps the 200 ms before payments notices it was already done (a known backlog item). Watch it settle:

```bash
promq 'max(saga_orders_stuck_pending)'; promq 'ALERTS{team="saga"}'
```

You should see the gauge fall back to `0` within a few minutes, with `OrdersStuckPending` resolving (and a `[RESOLVED]` email within about five minutes more). You will probably also see `SagaSLOFastBurn` `firing` and `SagaSLOSlowBurn` `pending`: every order that waited out the outage reached its final state long after 5 seconds, so for the SLO they are all bad events. That is correct, and the clearest picture of what the outage cost customers. Each burn alert needs both its long and its short window over the threshold, so it resolves once the short window is clean again: fast burn about 5 minutes after the backlog is gone, slow burn (which may fire meanwhile, after its `for: 15m`, with its own email) after 30 minutes. That short window is there exactly so an alert stops soon after the fix (day-3 tour, section 5).

## 4. Argo CD: Git is what runs

**What it is.** Argo CD runs four pods in `argocd`: the **application controller** (compares Git with the cluster and applies the difference), the **repo server** (clones the repository and renders the Helm charts), the **API server** (the UI and API) and Redis (a cache). An **Application** is Argo CD's object that says "render this path of this repository at this revision, and apply it to this namespace".

The workload uses the **app-of-apps** pattern. `make local-up` applies one Application, `root`. It renders `deploy/argocd/apps`, a small chart whose output is seven more Applications:

```
root (deploy/argocd/apps)
├── svc-orders, svc-inventory, svc-payments, svc-notifications, svc-loadgen
│       charts/service + deploy/local/values/<name>.yaml  ──► namespace shop
├── saga-observability   charts/saga-observability          ──► namespace observability
└── policies             deploy/local/policies               ──► every namespace's NetworkPolicies
```

All eight sync automatically, with **prune** (an object that disappears from Git is deleted from the cluster) and **self-heal** (a change made behind Argo CD's back is reverted). The platform under the workload (Postgres, NATS, Mailpit, the observability stack, Envoy Gateway, Argo CD itself) is still installed by `scripts/local-up.sh`; [ADR 0010](adr/0010-gitops-pull-model.md) explains why (the platform charts install CRDs that the workload needs, and ordering that in Argo CD is work left for later).

**Defined in** `deploy/argocd/root.yaml`, `deploy/argocd/apps/` (values and `templates/applications.yaml`), `deploy/local/values/argo-cd.yaml` (the install), `ARGOCD_CHART_VERSION` in `deploy/local/versions.env`, and `install_argocd`, `apply_root_app` and `wait_apps` in `scripts/local-up.sh`. `scripts/chart-test.sh` renders the apps chart and checks all seven Applications offline.

```bash
kubectl --context kind-order-saga get applications -n argocd -o custom-columns='NAME:.metadata.name,REVISION:.spec.source.targetRevision,SYNCED-TO:.status.sync.revision,SYNC:.status.sync.status,HEALTH:.status.health.status'
```

You should see eight rows, all `Synced` and `Healthy`, all with the branch from the start of this tour as `REVISION` and the same commit as `SYNCED-TO`. That commit is the newest one on the branch on GitHub:

```bash
git fetch -q origin && git rev-parse "origin/$(kubectl --context kind-order-saga get application root -n argocd -o jsonpath='{.spec.source.targetRevision}')"
```

You should see the same 40-character commit id (unless you pushed in the last few minutes: Argo CD polls GitHub every 120 seconds plus up to 60 seconds of jitter, so a push usually arrives within three minutes; it has been measured from 25 seconds to 4 minutes). **Sync** status compares Git with the cluster (`Synced` or `OutOfSync`). **Health** says whether what was applied works (`Healthy`, `Progressing` while a rollout is under way, `Degraded` when it failed). They are independent: section 6 shows an Application that is `Synced` but not `Healthy`.

### The UI

In the second terminal (stop `make prom` first, or use another terminal):

```bash
make argocd
```

You should see `Argo CD on https://localhost:8443, user admin. Initial password:`, the `kubectl` command that reads the password, and `Forwarding from 127.0.0.1:8443 -> 8080`. Run the printed command in the first terminal to read the password (it was generated by Argo CD at install time and is not in Git; do not paste it anywhere). Open `https://localhost:8443`, accept the self-signed certificate, log in as `admin`. You see the eight Applications as tiles. Open `root`: its tree shows the seven child Applications. Open `svc-orders`: the tree goes Deployment → ReplicaSet → two pods, plus the Service and the ServiceAccount, each with a sync and health icon. **History and rollback** shows each commit it applied.

### Self-heal

Scale `orders` behind Argo CD's back, and look again two seconds later:

```bash
kubectl --context kind-order-saga scale deployment orders -n shop --replicas=1; sleep 2; kubectl --context kind-order-saga get deployment orders -n shop -o jsonpath='{.spec.replicas}{"\n"}'
```

You should see `deployment.apps/orders scaled`, then `2`. Git says `replicaCount: 2` (`deploy/local/values/orders.yaml`), so Argo CD put it back. It did not wait for the Git poll: the controller watches the live objects, and reacts to a change within a couple of seconds (measured: 2 s). Nothing to restore.

Self-heal only reverts fields that Git sets. `make local-reload` restarts the Deployments with `kubectl rollout restart`, which adds an annotation Git does not mention, so Argo CD leaves the restart alone (ADR 0010).

### Prune

Prune deletes what Argo CD manages but Git no longer has. Argo CD knows what it manages by an annotation it puts on every object it applies:

```bash
kubectl --context kind-order-saga get deployment orders -n shop -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/tracking-id}{"\n"}'
```

You should see `svc-orders:apps/Deployment:shop/orders`: Application, kind, namespace and name. Normally an object disappears from Git because someone deleted the file or a template; to see prune without a commit, create an object that claims to belong to `svc-orders` although Git has no such thing:

```bash
kubectl --context kind-order-saga apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: tour-extra
  namespace: shop
  annotations:
    argocd.argoproj.io/tracking-id: svc-orders:/ConfigMap:shop/tour-extra
data:
  note: made by hand
EOF
sleep 5; kubectl --context kind-order-saga get configmap tour-extra -n shop
```

You should see `configmap/tour-extra created`, then `Error from server (NotFound): configmaps "tour-extra" not found`: `svc-orders` went `OutOfSync` for a second and the automatic sync pruned it. Now the same without the annotation:

```bash
kubectl --context kind-order-saga create configmap tour-untracked -n shop --from-literal=note=made-by-hand; sleep 5; kubectl --context kind-order-saga get configmap tour-untracked -n shop
```

You should see `configmap/tour-untracked created` and then the ConfigMap still there. Argo CD never touches what it does not track: prune is not a cleaner of everything Git lacks, only of what it once applied (or what claims to be its own).

**Restore.**

```bash
kubectl --context kind-order-saga delete configmap tour-untracked -n shop
```

You should see `configmap "tour-untracked" deleted`.

### Pausing Argo CD

Section 3 paused `root` and `policies` to break something on purpose. That is the general recipe for any experiment on an Argo-managed object, for example the day-3 payment outage: remove `automated` from `root` and from the child Application, change the object with `kubectl`, and when you are done patch `root` back (the command in section 3's Restore). `root` restores the child's sync policy, and the child then reverts your change. Never leave `root` paused: nothing would deploy.

## 5. The release path: from a commit to a rollout

**What it is.** Two GitHub Actions workflows, and Argo CD at the end. No workflow can reach the cluster: CI holds no kubeconfig and no cloud credentials, only permission to push images to GHCR (GitHub's container registry) and to commit to `main`. The cluster pulls.

```
git push ──► ci.yml (every push and PR): lint, go test, chart-test + rules-test, secrets scan, bump-script test, actionlint
PR ───────► release.yml: build 5 images (amd64 + arm64), Trivy scan; nothing pushed
gh workflow run release.yml --ref main
          ──► release.yml: build, scan, push ghcr.io/jmmorell/k8s-order-saga/<svc>:<short-sha> (+ :latest-main)
          ──► commit "chore(deploy): image tag <sha> [skip ci]" to main (deploy/local/values/*.yaml)
          ──► Argo CD sees the commit (a few minutes) ──► rolling update of the five Deployments, pulled from GHCR
```

**Defined in** `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `scripts/bump-image-tag.sh` (rewrites the one `image.tag` line in each values file, tested by `scripts/bump-image-tag_test.sh`), and the tool versions in `deploy/local/versions.env`. Every action is pinned by commit SHA, the Trivy and actionlint images by digest.

**CI on push.** Every push runs `ci.yml`:

```bash
gh run list --workflow ci.yml --branch "$(git branch --show-current)" --limit 3
```

You should see three rows starting `completed success`, one per recent push, each taking about two to three minutes. `gh run view <id>` (the number in the row) lists its six jobs: `lint`, `test`, `charts`, `secrets`, `bump-script` and `actionlint`. The same checks run locally: `make lint`, `make test`, `make chart-test`, `make rules-test`, `make secrets-scan`, `make ci-local`.

**`release.yml` on a pull request.** Once the pull request is open, every push to it also runs `release.yml`, but only half of it: build both architectures, build a copy for the runner and scan it with Trivy (section 7). A fixable HIGH or CRITICAL vulnerability fails the check before anything is merged. Nothing is pushed and nothing is committed; the push and bump steps run only on `workflow_dispatch`. Look at it with `gh pr checks` on the branch: five `image (<service>)` checks next to the `ci` ones.

### After the PR is merged: dispatch a release

A release is a manual decision (ADR 0010), from `main` only (the workflow fails at once on any other ref). Before the merge, `release.yml` is not on the default branch and GitHub answers these commands with `HTTP 404: workflow release.yml not found on the default branch`.

```bash
gh workflow run release.yml --ref main
```

You should see `✓ Created workflow_dispatch event for release.yml at main` (and a link to the run). Watch it:

```bash
gh run watch "$(gh run list --workflow release.yml --event workflow_dispatch --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
```

You should see the five `image (<service>)` jobs, then `bump`, each turning into a check mark; the first run takes several minutes (nothing is cached yet). If `gh run list` returns nothing, the run has not appeared yet: wait a few seconds and run it again. Then the commit it made:

```bash
git pull -q && git log -1 --format='%h %an %s' -- deploy/local/values
```

You should see `<hash> github-actions[bot] chore(deploy): image tag <7 hex> [skip ci]`, and `git show HEAD` shows one changed `tag:` line in each of the five values files.

**Make the packages public (first release only).** The first push creates five packages in GHCR, and GitHub may create them **private** even though the repository is public. Then neither kind's nodes nor anyone else can pull them without credentials, and the new pods sit in `ErrImagePull`. Check without logging in:

```bash
curl -s -o /dev/null -w '%{http_code}\n' 'https://ghcr.io/token?scope=repository:jmmorell/k8s-order-saga/orders:pull'
```

You should see `200` when the package can be pulled anonymously and `403` when it is private (or does not exist yet). If `403`: on GitHub, open your profile, **Packages**, then each `k8s-order-saga/<service>` package, **Package settings**, **Danger Zone**, **Change visibility**, **Public**. Do it for all five, then run the `curl` again for each.

**Watch Argo CD roll it out.** Within a few minutes of the bump commit (sooner if you press **Refresh** on an Application in the UI), the five `svc-*` Applications go `OutOfSync`, sync, show `Progressing` while the new pods start, and return to `Synced` and `Healthy`:

```bash
kubectl --context kind-order-saga get pods -n shop -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image'
```

You should see every pod on `ghcr.io/jmmorell/k8s-order-saga/<service>:<the 7 hex of the bump>` instead of `:dev`. From now on the nodes pull these images from GHCR; `make local-reload` only rebuilds services whose values still say `dev` (none, after a release), and a release tag is never built locally, so the cluster always runs exactly what that tag names.

## 6. A bad rollout, and the rollback with `git revert`

**What it is.** Git is the only way in, so it is also the way back. A release that commits a broken image tag is undone by reverting the commit; nobody touches the cluster. This drill commits an image tag that does not exist and measures what happens. It pushes to the branch Argo CD tracks (the one from the start of this tour), so do it on `main` only after the merge, or on the day-4 branch before it.

Change the payments tag to one that was never built, commit and push:

```bash
perl -pi -e 's/^  tag: .*/  tag: "0000000"/' deploy/local/values/payments.yaml && git commit -q -m 'drill: payments image tag 0000000 (does not exist)' deploy/local/values/payments.yaml && git push -q && date -u +%T
```

You should see the time in UTC and no other output; `git show --stat HEAD` shows `deploy/local/values/payments.yaml | 2 +-`. In the second terminal, watch the Application:

```bash
kubectl --context kind-order-saga get application svc-payments -n argocd -w
```

You should see, within a few minutes (measured: 25 s and 82 s), `OutOfSync`, then `Synced` with health `Progressing`. It stays `Progressing`. Look at the pods:

```bash
kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=payments -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image,READY:.status.containerStatuses[0].ready,WAITING:.status.containerStatuses[0].state.waiting.reason'
```

You should see two pods: the new one on `...payments:0000000`, not ready, waiting with `ErrImagePull` or `ImagePullBackOff`, and the old one on the previous tag, ready, with no waiting reason. The old pod keeps serving because a rolling update only removes an old pod after its replacement is ready (with one replica, `maxUnavailable` 25 % rounds down to 0). The saga does not notice:

```bash
make local-smoke
```

You should see `SMOKE OK`. **Why it says Progressing, not Degraded.** Argo CD takes its health from the Deployment, and the Deployment only declares failure once its `progressDeadlineSeconds` (600 s, the default) has passed without progress. About 10 minutes after the sync, the watch shows `Degraded` (measured: 11.5 minutes after the push). An alert or a drill script must expect those 10 minutes.

**Rollback.** Revert the commit and push. Do not wait for `Degraded`; this works at any point:

```bash
git revert --no-edit HEAD && git push -q && date -u +%T
```

You should see `[<branch> <hash>] Revert "drill: payments image tag 0000000 (does not exist)"` and the time. Within a few minutes the watch shows `OutOfSync`, then `Synced` and `Healthy` (measured: 40 s and 4 minutes after the push, Healthy seconds after the sync). The broken ReplicaSet is scaled to zero, and only the original pod remains; it was never restarted:

```bash
kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=payments
```

You should see one `payments-...` pod, `1/1 Running`, with the same name and age as before the drill. Stop the watch with Ctrl-C. Both commits stay in the history: the record of what was deployed and undone, and when. `git log --oneline -3` shows the revert on top of the drill.

## 7. Trivy: reading a scan

**What it is.** Trivy compares what is installed in an image with databases of known vulnerabilities (CVEs). For our images it looks at two things: the operating-system packages of the distroless base (Debian 12, five packages) and the Go binary, whose embedded module list (`go version -m`) tells it every library and the Go standard library version. `release.yml` runs it with `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`: the job fails on any **fixable** HIGH or CRITICAL finding.

Scan the `orders` image `make local-up` built, with the same pinned Trivy and the same flags as the workflow (each run downloads Trivy's vulnerability database first, about 20 seconds, because the container keeps no cache):

```bash
(. deploy/local/versions.env && docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$TRIVY_IMAGE" image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --no-progress ghcr.io/jmmorell/k8s-order-saga/orders:dev); echo "exit $?"
```

You should see some `INFO` lines (`Detected OS family="debian" version="12.15"`, `[gobinary] Detecting vulnerabilities...`), then a **Report Summary** table with two rows, `ghcr.io/jmmorell/k8s-order-saga/orders:dev (debian 12.15)` (`debian`) and `app` (`gobinary`), both with `0` vulnerabilities, and `exit 0`. A clean image prints only the summary.

To see what a finding looks like, scan an image that is not built here, Postgres (the tag `postgres:17-alpine` moves, so the numbers depend on when your copy was pulled):

```bash
(. deploy/local/versions.env && docker run --rm -v /var/run/docker.sock:/var/run/docker.sock "$TRIVY_IMAGE" image --severity HIGH,CRITICAL --ignore-unfixed --scanners vuln -q postgres:17-alpine) | head -30
```

You should see a summary with an `alpine` row and a `usr/local/bin/gosu` (`gobinary`) row, then a table per target headed `Total: <n> (HIGH: <n>, CRITICAL: <n>)`, with the columns **Library** (the package, for example `krb5-libs`), **Vulnerability** (the CVE id), **Severity**, **Status**, **Installed Version**, **Fixed Version** and **Title** (with a link to the advisory). With a copy pulled some weeks ago there were 20 HIGH findings in Alpine packages (`krb5-libs` and others) and 26 (one CRITICAL) in the Go standard library compiled into `gosu`, every one with Status `fixed`.

**What "fixable HIGH/CRITICAL" means.**

- **Severity** is the rating from the distribution or the advisory database: LOW, MEDIUM, HIGH or CRITICAL. The gate ignores LOW and MEDIUM, which would otherwise fail almost every build for little benefit.
- **Status `fixed`** means a newer version of that package with the fix exists (**Fixed Version**). Rebuilding on a newer base image, or bumping the Go toolchain for a standard-library finding, makes it go away. **`affected`** or **`will_not_fix`** means no fix exists yet; failing the build on those blocks every release with nothing you can do about it, so `--ignore-unfixed` hides them.
- So the gate reads: "do not ship a serious vulnerability that a rebuild or an upgrade would remove". An unfixed one remains a risk to judge by hand.

Our images score 0 because the base is distroless (five OS packages, no shell) and the binary is built with a current Go. Postgres, NATS and Mailpit come from public images, are not scanned by any workflow, and are pinned by a moving tag: that gap is in the backlog.

## 8. What is still missing

Day 4 is the security and delivery baseline, not the end. Deferred to iteration 2, each a deliberate cut:

- **Image signing and provenance.** Nothing proves that the image Argo CD deploys is the one `release.yml` built and scanned. Signing with cosign (keyless, through GitHub's OIDC identity) plus an admission policy that rejects unsigned images would. Related: the release pushes a rebuild of what it scanned rather than the scanned artifact itself, and base images are referenced by tag, not digest.
- **A secret manager.** The Postgres and Grafana passwords are generated by `local-up.sh` into Kubernetes Secrets that exist only in the cluster. On GKE they belong in Secret Manager, synced by External Secrets or mounted by the Secret Manager CSI driver, so the cluster can be rebuilt without the script inventing new ones.
- **Private networking.** On GKE: a private cluster with no public node IPs, a private control-plane endpoint, and Argo CD's UI behind IAP or a port-forward, never a public load balancer. Locally nothing is exposed except `localhost:8080`.
- **The platform charts in Argo CD.** Postgres, NATS, Mailpit, the observability stack and Envoy Gateway are still installed by `local-up.sh`, so a platform upgrade is not yet a Git-driven sync (ADR 0010, CRD ordering).
- **NetworkPolicies on GKE.** Dataplane V2 (Cilium) treats host traffic, probes and port-forwards differently from kindnet. The policies, `make policy-test` and the webhook allow must be re-verified there, and the in-cluster Postgres allow becomes egress to the Cloud SQL Auth Proxy.

The smaller findings, with their reasons, are in [docs/backlog.md](backlog.md) under "Day 4".

## Before you leave

Stop every port-forward and watch with Ctrl-C. Check that every exercise is undone. Argo CD runs everything, nothing is paused, and every Application is green:

```bash
kubectl --context kind-order-saga get applications -n argocd -o custom-columns='NAME:.metadata.name,AUTO-SYNC:.spec.syncPolicy.automated.selfHeal,SYNC:.status.sync.status,HEALTH:.status.health.status'
```

You should see eight rows, each with `true`, `Synced` and `Healthy`.

If you did section 3 less than half an hour ago, `promq 'ALERTS{team="saga"}'` may still list `SagaSLOSlowBurn`; it resolves by itself, as section 3 explains.

```bash
kubectl --context kind-order-saga get networkpolicy -A --no-headers | wc -l; kubectl --context kind-order-saga get configmap -n shop | grep tour-; git status --short deploy/
```

You should see `19`, and nothing else: no tour ConfigMap left and no uncommitted change under `deploy/` (section 6's two commits are pushed). Then the pieces sections 3 and 6 touched:

```bash
kubectl --context kind-order-saga get networkpolicy egress-nats -n shop -o jsonpath='{.spec.podSelector.matchExpressions[0].values}{"\n"}'; kubectl --context kind-order-saga get deployment payments -n shop
```

You should see `["orders","inventory","payments","notifications"]` and `payments` `1/1`.

```bash
make local-smoke && make policy-test && make obs-check
```

You should see `SMOKE OK`, `POLICY OK`, then one `ok` line per query and `OBS OK`.

## Going further

[ADR 0009](adr/0009-network-policies-and-pod-security.md) covers Pod Security and the NetworkPolicies: the levels per namespace, what bypasses the policies on kindnet, and the extra webhook allow found while testing. [ADR 0010](adr/0010-gitops-pull-model.md) covers the pull model: why CI never gets cluster credentials, the manual release, the bootstrap/workload split, self-heal versus `make local-reload`, and the bad-rollout numbers. When you have finished this tour and the Day 4 checkpoint in the README, you should be able to defend both.
