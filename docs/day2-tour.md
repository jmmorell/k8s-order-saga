# Day 2 tour: the cluster, piece by piece

You know GCP and the terminal, but you have not configured Kubernetes by hand. This tour walks you through everything day 2 built, one object at a time. For each piece you see what it is, which file defines it, how to look at it live, and (where it is safe) how to break it and watch it heal.

You need the cluster running (`make local-up`, then `make local-smoke` prints `SMOKE OK`). Every command passes `--context kind-order-saga`, because this machine also has other Kubernetes contexts and you never want to touch the wrong cluster. The commands are written for zsh and bash and can be copied as they are.

Some commands run forever (`kubectl get pods -w`). Run those in a second terminal and stop them with Ctrl-C.

## A few words first

Kubernetes (K8s) runs containers for you on a group of machines. You do not tell it "start this container"; you describe the state you want ("two copies of orders, always"), and K8s keeps reality equal to that description. Everything is an object with a `kind`, a `name` and a `spec` (what you want), and K8s fills in a `status` (what is true now). The objects live in the API server, and `kubectl` is the command line that talks to it.

Three words come back all the time:

- A **pod** is one running copy of your program (one or more containers that share a network address). Pods are disposable.
- A **label** is a key/value tag on an object. Objects find each other by labels, never by name.
- A **namespace** is a folder inside the cluster. Names only have to be unique inside one.

## 1. The cluster

**What it is.** A cluster is a set of machines (nodes). One node, the control plane, runs the brain: the API server, the scheduler (which decides where each pod runs) and etcd (the database holding every object). The other nodes, the workers, run your pods. Here the nodes are Docker containers, created by kind ("Kubernetes in Docker").

**Why three.** One control plane and two workers is the smallest shape where scheduling means something: pods spread over two machines, and you can see what happens to a service when its pods sit on different nodes. By default the control plane refuses ordinary pods (it has a taint, a "keep out" marker), so the workers carry the load.

**Defined in** `deploy/local/kind-cluster.yaml`. The Kubernetes version is pinned there by image digest (v1.37.0), so a rebuild in a year gives the same cluster. See ADR 0005.

List the nodes:

```bash
kubectl --context kind-order-saga get nodes -o wide
```

You should see three nodes, all `Ready`, version `v1.37.0`, one with role `control-plane`.

Now look at where they really are:

```bash
docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Ports}}'
```

You should see three containers named `order-saga-control-plane`, `order-saga-worker` and `order-saga-worker2`. Only the control plane publishes ports: `127.0.0.1:8080->30080/tcp` is the way in from your machine (section 8), and the other port is the Kubernetes API that `kubectl` talks to.

See the control-plane taint that keeps ordinary pods away:

```bash
kubectl --context kind-order-saga get node order-saga-control-plane -o jsonpath='{.spec.taints}{"\n"}'
```

You should see one taint with the key `node-role.kubernetes.io/control-plane` and effect `NoSchedule`.

See which node each of your pods landed on:

```bash
kubectl --context kind-order-saga get pods -n shop -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName
```

You should see only `order-saga-worker` and `order-saga-worker2`, never the control plane. The two `orders` pods are usually on different workers, but the scheduler is free to choose.

## 2. Namespaces

**What it is.** A namespace groups objects and gives names a scope. The same name can exist in two namespaces without clashing, which matters later because the `postgres` Secret exists in both `platform` and `shop`.

**Defined in** `deploy/local/platform/namespaces.yaml`. The `kube-system`, `kube-public`, `kube-node-lease` and `default` namespaces are created by Kubernetes itself, and `local-path-storage` by kind.

| Namespace | What lives there |
|---|---|
| `shop` | The five Go services, the Gateway and the HTTPRoute |
| `platform` | NATS, Postgres, Mailpit: things the services depend on |
| `envoy-gateway-system` | The Envoy Gateway controller and the Envoy proxy it creates |

```bash
kubectl --context kind-order-saga get namespaces
```

You should see `shop`, `platform` and `envoy-gateway-system` next to the built-in ones.

```bash
kubectl --context kind-order-saga get pods --all-namespaces -o wide
```

You should see every pod in the cluster with its node and IP. Everything is `Running` and `READY` is `1/1` (`2/2` for `nats-0` and the Envoy proxy, which have two containers each).

Most commands only see one namespace. If a `kubectl get` returns "No resources found", you forgot `-n shop` or `-n platform`.

## 3. A Deployment, end to end

**What it is.** A Deployment says "keep N identical copies of this pod running". It does not manage pods directly: it creates a ReplicaSet, and the ReplicaSet creates the pods. When you change the pod template (say, a new image), the Deployment creates a new ReplicaSet and slowly moves pods over. That is how rolling updates work.

**Defined in** the chart `charts/service`. A Helm chart is a folder of templates plus default values; Helm fills the templates in and sends the result to the cluster. One chart serves all five services (ADR 0006): the release name is the service name, and `deploy/local/values/<service>.yaml` holds what differs (for `orders`: two replicas and its environment). The Deployment template is `charts/service/templates/deployment.yaml`.

Render the chart without touching the cluster:

```bash
helm template orders charts/service -f deploy/local/values/orders.yaml --set image.repository=k8s-order-saga/orders
```

You should see three YAML documents: a ServiceAccount, a Service and a Deployment named `orders` with `replicas: 2`. This is exactly what `scripts/local-up.sh` installs. Compare it with `charts/service/templates/deployment.yaml` and notice which blanks the values filled in.

Ask Helm what is installed:

```bash
helm --kube-context kind-order-saga list --all-namespaces
```

You should see the releases `orders`, `inventory`, `payments`, `notifications` and `loadgen` in `shop`, `nats` in `platform` and `eg` in `envoy-gateway-system`.

Now follow the chain from Deployment down to pods:

```bash
kubectl --context kind-order-saga get deployment,replicaset,pods -n shop -l app.kubernetes.io/name=orders
```

You should see one Deployment `orders` (`2/2`), one ReplicaSet `orders-<hash>` (desired 2, ready 2) and two pods named `orders-<hash>-<suffix>`. The name tells you the ancestry.

The pods carry labels, and the Deployment finds its pods by one of them:

```bash
kubectl --context kind-order-saga get pods -n shop --show-labels
```

You should see `app.kubernetes.io/name=<service>` on each pod (this one is the selector), plus `app.kubernetes.io/part-of=order-saga` on all of them.

The selector `-l app.kubernetes.io/part-of=order-saga` addresses all five services at once, which lets you follow one order across all of them. Place an order and keep its id:

```bash
ORDER_ID=$(curl -s -X POST localhost:8080/orders -H 'Content-Type: application/json' -d '{"customer_email":"tour@example.local","items":[{"sku":"SKU-001","qty":1}]}' | sed -E 's/.*"id":"([^"]+)".*/\1/'); echo "$ORDER_ID"
```

You should see the new order id.

```bash
kubectl --context kind-order-saga logs -n shop -l app.kubernetes.io/part-of=order-saga --prefix=false --tail=-1 | grep "$ORDER_ID" | sort
```

You should see the same story as the day-1 checkpoint: `order created` (orders), `stock reserved` (inventory), `payment succeeded` (payments), `order confirmed` (orders), `email sent` (notifications). Each line is a JSON object starting with its timestamp, so `sort` puts them in order. If payments declined this order (it does that for about 10 % on purpose), you see `payment failed` and `order cancelled` instead; place another one. If the grep prints nothing, wait two seconds and run it again.

### Break it and watch: delete a pod

In a second terminal, start watching the `orders` pods (stop it with Ctrl-C when you are done):

```bash
kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=orders -w
```

You should see the two pods, then nothing until you act. In the first terminal, pick one pod of orders and delete it:

```bash
POD=$(kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=orders -o jsonpath='{.items[0].metadata.name}'); kubectl --context kind-order-saga delete pod "$POD" -n shop
```

You should see `pod "orders-..." deleted`. In the watch terminal the pod goes `Terminating` while a new one with a different suffix appears, goes `Running` and finally `1/1`. The ReplicaSet noticed "desired 2, found 1" and made a replacement. You never restart anything by hand.

There is nothing to restore: the Deployment already did it. Confirm:

```bash
kubectl --context kind-order-saga get deployment orders -n shop
```

You should see `READY 2/2`.

## 4. Configuration and the Secret

**What it is.** The Go services read all their settings from environment variables. Plain settings are written in the values file; the database password comes from a Secret, which is a Kubernetes object that holds small sensitive values.

**Defined in** `deploy/local/values/orders.yaml` (and its siblings), turned into container environment by `charts/service/templates/deployment.yaml`. The Secret itself is created by `scripts/local-up.sh` (function `ensure_secrets`).

See the values for orders:

```bash
cat deploy/local/values/orders.yaml
```

You should see `secretEnv` (a reference: "the variable `POSTGRES_PASSWORD` comes from the Secret `postgres`, key `password`") and `env` with `DATABASE_URL` and `NATS_URL`.

Notice `DATABASE_URL` contains `$(POSTGRES_PASSWORD)`. That is Kubernetes' own expansion: for a variable value, the kubelet (the agent on each node) replaces `$(NAME)` with the value of another variable defined earlier in the same container. The chart renders `secretEnv` first for that reason. The password never appears in Git, in the values file or in the Helm release. See the environment as stored in the Deployment:

```bash
kubectl --context kind-order-saga get deployment orders -n shop -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"\t"}{.value}{.valueFrom}{"\n"}{end}'
```

You should see `POSTGRES_PASSWORD` with a `secretKeyRef` (name `postgres`, key `password`), `HTTP_ADDR` `:8080`, and `DATABASE_URL` still containing the literal text `$(POSTGRES_PASSWORD)`. The expansion happens only inside the running container.

Look at the Secret objects (the password is not printed):

```bash
kubectl --context kind-order-saga get secret postgres -n shop
```

You should see a Secret of type `Opaque` with `DATA 1`. The same Secret exists in `platform`, because a Secret cannot be read across namespaces: Postgres reads it there, the services read the copy in `shop`.

Secrets are only base64-encoded, not encrypted, so anyone who may read them can decode them. This is how you check that the password has the expected length without printing it:

```bash
kubectl --context kind-order-saga get secret postgres -n shop -o jsonpath='{.data.password}' | base64 --decode | wc -c
```

You should see `32` (16 random bytes in hex).

**Why a script creates it and Git does not hold it.** Anything in Git stays there forever and is copied to every clone. A password generated on first run (`openssl rand -hex 16`) exists only in your cluster, and `make local-down` throws it away together with the data it protects. The script creates the Secret only if it is missing, so running `make local-up` again keeps the password that the existing database already uses. On GKE you would not use a script here but Secret Manager or a similar service; the principle is the same.

## 5. Probes

**What it is.** A probe is a check the kubelet runs against a container. This chart defines two on every service, both HTTP on port 8080:

- **Liveness** (`GET /healthz`): "is the process alive?" If it fails three times in a row, the kubelet kills the container and starts a new one (a restart).
- **Readiness** (`GET /readyz`): "can it take traffic right now?" If it fails, the pod stays running but is taken out of its Service, so no traffic reaches it. No restart.

The split matters. A dependency being down (NATS, the database) is not fixed by restarting your service, so it belongs in readiness. In these services `/healthz` always answers 200, and `/readyz` answers 503 with the failed check names when NATS or the database is unreachable. `loadgen` has no dependency checks, so its `/readyz` is always 200.

**Defined in** `charts/service/templates/deployment.yaml` (probes) and `internal/platform/httpx/health.go` (what the endpoints answer).

See both probes of a running pod:

```bash
kubectl --context kind-order-saga describe pod -n shop -l app.kubernetes.io/name=orders | grep -E "Liveness:|Readiness:"
```

You should see each probe printed twice (one line per pod), `http-get http://:http/healthz` and `/readyz`, with `period=10s` and `#failure=3`.

### Break it and watch: take NATS away

In a second terminal, watch the pods of the `shop` namespace (stop it with Ctrl-C afterwards):

```bash
kubectl --context kind-order-saga get pods -n shop -w
```

You should see all six pods `1/1`. In the first terminal, scale the NATS StatefulSet to zero pods (a StatefulSet is the Deployment's cousin for stateful programs; see section 7):

```bash
kubectl --context kind-order-saga scale statefulset nats -n platform --replicas=0
```

You should see `statefulset.apps/nats scaled`. Now wait. Within about a minute the readiness probes fail three times in a row (a probe runs every 10 seconds), and `inventory`, `notifications`, `payments` and both `orders` pods change to `0/1`. `loadgen` stays `1/1` because it has no dependency checks. Look at the `RESTARTS` column: it stays at `0`. The pods are not broken; they are waiting.

While it is down, ask for an order:

```bash
curl -s -w ' %{http_code}\n' -X POST localhost:8080/orders -H 'Content-Type: application/json' -d '{"customer_email":"tour@example.local","items":[{"sku":"SKU-001","qty":1}]}'
```

You should see `{"error":"temporarily unavailable"}` and `503`: orders refuses to accept an order it cannot publish. Check what loadgen sees:

```bash
kubectl --context kind-order-saga logs -n shop deploy/loadgen --tail=2
```

You should see a line like `responses in the last 30s` with a `"503"` count next to the usual `"202"` count.

See why the Service stopped sending traffic to the `orders` pods:

```bash
kubectl --context kind-order-saga get endpointslices -n shop -l kubernetes.io/service-name=orders -o jsonpath='{range .items[0].endpoints[*]}{.addresses[0]} ready={.conditions.ready}{"\n"}{end}'
```

You should see both pod addresses with `ready=false`. A pod that is not ready is left out of the Service.

**Restore.** Scale NATS back to one pod:

```bash
kubectl --context kind-order-saga scale statefulset nats -n platform --replicas=1
```

You should see `statefulset.apps/nats scaled`. Wait for it:

```bash
kubectl --context kind-order-saga rollout status statefulset/nats -n platform --timeout=120s
```

You should see `partitioned roll out complete: 1 new pods have been updated...`. The services reconnect on their own, and within about 10 to 20 seconds every pod in the watch is back to `1/1` with `RESTARTS` still `0`. Stop the watch with Ctrl-C, then confirm that the whole saga works again:

```bash
make local-smoke
```

You should see `SMOKE OK`. The messages that were accepted before NATS stopped are still there: NATS keeps them on its volume (section 7).

## 6. Service discovery

**What it is.** Pods come and go and their IP addresses change every time, so nobody should talk to a pod directly. A **Service** is a stable name and IP in front of a changing set of pods, chosen by label. Inside the cluster, DNS gives every Service a name: `<service>.<namespace>.svc.cluster.local`. That is why `orders` reaches Postgres as `postgres.platform.svc.cluster.local:5432`, and the other services reach orders as `orders.shop.svc.cluster.local:8080`.

**Defined in** `charts/service/templates/service.yaml` for the services, and `deploy/local/platform/*.yaml` and the NATS chart for the platform pieces. The type is `ClusterIP`, the default: reachable only from inside the cluster.

```bash
kubectl --context kind-order-saga get services --all-namespaces
```

You should see a Service per component. Only one has a `NodePort` type (`envoy-shop-shop-...`, section 8); the rest are `ClusterIP`. Note that `nats-headless` has `CLUSTER-IP None`: that is a "headless" Service that gives each StatefulSet pod its own DNS name instead of one shared address.

A Service does not hold pod addresses itself. Another object does, the EndpointSlice, which Kubernetes keeps in sync by selecting the pods that match the Service's labels and are ready:

```bash
kubectl --context kind-order-saga get endpointslices -n shop -l kubernetes.io/service-name=orders
```

You should see one slice with `ENDPOINTS` listing two IPs on port 8080: the two ready `orders` pods.

Compare those IPs with the pods:

```bash
kubectl --context kind-order-saga get pods -n shop -l app.kubernetes.io/name=orders -o wide
```

You should see the same two IPs in the `IP` column.

Look at the Service itself:

```bash
kubectl --context kind-order-saga describe service orders -n shop
```

You should see `Selector: app.kubernetes.io/name=orders`, `Type: ClusterIP`, the `Endpoints` line with the two pod IPs and `Events: <none>`.

Delete a pod again (as in section 3) and run the `get endpointslices` command a few times: the dead pod's address vanishes and the new one appears, without anyone editing the Service.

## 7. Stateful workloads

**What it is.** A Deployment's pods are interchangeable and anonymous. A database is not. A **StatefulSet** gives each pod a stable name (`postgres-0`, never `postgres-7f9c...`) and its own disk, which stays attached to that name. A **PersistentVolumeClaim** (PVC) is the pod's request for a disk ("1 GiB, one node at a time"); a **PersistentVolume** (PV) is the disk that fulfils it; the **StorageClass** (`standard` here) says how disks are made. In kind they are just directories on the node.

**Defined in** `deploy/local/platform/postgres.yaml` (Postgres, plain manifest) and the official NATS chart configured by `deploy/local/values/nats.yaml` (NATS JetStream also has a PVC). The `volumeClaimTemplates` block in the StatefulSet creates `data-postgres-0` for you.

```bash
kubectl --context kind-order-saga get statefulset,pvc -n platform
```

You should see the StatefulSets `nats` and `postgres` (`1/1`) and the claims `data-postgres-0` and `nats-js-nats-0`, both `Bound`, `1Gi`.

```bash
kubectl --context kind-order-saga get storageclass
```

You should see `standard (default)` with provisioner `rancher.io/local-path` and reclaim policy `Delete`: when a claim is deleted, its disk goes too.

### Break it and watch: delete the database pod

Count the orders in the database:

```bash
kubectl --context kind-order-saga exec -n platform postgres-0 -- psql -U postgres -d orders -tAc "select count(*) from orders"
```

You should see a number (it grows steadily because loadgen places orders). Delete the pod:

```bash
kubectl --context kind-order-saga delete pod postgres-0 -n platform
```

You should see `pod "postgres-0" deleted`. The StatefulSet recreates a pod with the same name and re-attaches the same claim. Wait for it:

```bash
kubectl --context kind-order-saga wait --for=condition=Ready pod/postgres-0 -n platform --timeout=120s
```

You should see `pod/postgres-0 condition met`. Count again:

```bash
kubectl --context kind-order-saga exec -n platform postgres-0 -- psql -U postgres -d orders -tAc "select count(*) from orders"
```

You should see the same number or a larger one: the data survived the pod, because it lives on the PVC. While Postgres was gone, the services may have briefly failed their `db` readiness check; they recover on their own. Nothing to restore.

**What does not survive.** The PVC outlives the pod, but not the cluster. `make local-down` deletes the kind cluster, which deletes the node containers and with them every disk. The next `make local-up` starts with an empty database, a new Secret with a new password, and empty NATS streams. That is deliberate: the local cluster is disposable. On GKE the disk would be a Persistent Disk that survives cluster deletion unless you delete it, and the database would more likely be Cloud SQL.

## 8. The way in

**What it is.** Traffic from your machine takes this path:

```
curl localhost:8080
  -> kind port mapping (host 127.0.0.1:8080 -> control-plane node port 30080)
  -> NodePort Service 30080
  -> Envoy proxy pod
  -> HTTPRoute rule "/orders"
  -> Service orders:8080
  -> one of the two orders pods
```

Each hop is one object:

- **GatewayClass `eg`**: "Envoy Gateway implements gateways of this class." It also points to an `EnvoyProxy` object with settings for the proxy.
- **Gateway `shop`**: "listen for HTTP on port 80". Because it is of class `eg`, Envoy Gateway reacts by creating an Envoy proxy Deployment and Service in `envoy-gateway-system`, with generated names.
- **HTTPRoute `orders`**: the routing rule: paths starting with `/orders` go to Service `orders`, port 8080. Nothing else is routed.
- **EnvoyProxy `local`**: makes the proxy's Service a NodePort fixed on 30080 with `externalTrafficPolicy: Cluster`. A **NodePort** opens the same port on every node. `Cluster` means a node that receives traffic may forward it to a pod on another node. The default (`Local`) would drop traffic whenever the proxy pod is not on the control-plane node, which is the only node the kind mapping points to.

This is the Gateway API, the newer replacement for Ingress. The Gateway and HTTPRoute are generic; only the GatewayClass and EnvoyProxy are specific to Envoy and to kind (ADR 0007).

**Defined in** `deploy/local/gateway/` (`gatewayclass.yaml`, `envoyproxy.yaml`, `gateway.yaml`, `httproute.yaml`) and `deploy/local/kind-cluster.yaml` (the port mapping).

```bash
kubectl --context kind-order-saga get gatewayclass,gateway,httproute -A
```

You should see GatewayClass `eg` with `ACCEPTED True`, Gateway `shop` with `PROGRAMMED True` and an address, and HTTPRoute `orders`.

```bash
kubectl --context kind-order-saga get httproute orders -n shop -o jsonpath='{range .status.parents[0].conditions[*]}{.type}={.status}{"\n"}{end}'
```

You should see `Accepted=True` and `ResolvedRefs=True`: the route was accepted by the Gateway and its backend Service was found. When a route does not work, this is the first place to look.

Find the proxy that Envoy Gateway created, by label rather than by its generated name:

```bash
kubectl --context kind-order-saga get deployment,pods,service -n envoy-gateway-system -l gateway.envoyproxy.io/owning-gateway-name=shop
```

You should see one Deployment (`1/1`), one pod (`2/2`: the `envoy` container and a `shutdown-manager` sidecar) and one Service of type `NodePort` with `PORT(S)` `80:30080/TCP`.

Check that it is the only NodePort in the whole cluster:

```bash
kubectl --context kind-order-saga get services -A -o jsonpath='{range .items[?(@.spec.type=="NodePort")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

You should see exactly one line: `envoy-gateway-system/envoy-shop-shop-<hash>`.

Now use the road:

```bash
curl -s -w ' %{http_code}\n' localhost:8080/orders/00000000-0000-0000-0000-000000000000
```

You should see `{"error":"not found"}` and `404`. This 404 comes from the orders service itself (no order has that id), so it proves the whole path works.

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz
```

You should see `404`. This 404 comes from the Envoy proxy: only `/orders` is routed, so `/healthz` and `/readyz` are not reachable from outside. That is deliberate. Those endpoints belong to the kubelet, which calls the pods directly, and nobody on the outside needs them. It is also why `scripts/local-smoke.sh` checks readiness by asking for an unknown order and matching the `"error":"not found"` body.

Mailpit has no route either. To see the emails in a browser, open a temporary tunnel to its Service (stop it with Ctrl-C):

```bash
kubectl --context kind-order-saga port-forward -n platform svc/mailpit 8025:8025
```

You should see `Forwarding from 127.0.0.1:8025 -> 8025`. Open `http://localhost:8025` in a browser to see the order emails. `port-forward` is a debugging tool: it works through the API server and exposes nothing permanently.

## 9. Resources

**What it is.** Every container declares **requests** and **limits**, both for CPU and memory:

- A **request** is what the scheduler reserves when it places the pod. A node with no room left for the request will not get the pod.
- A **limit** is the ceiling at runtime. A container using more CPU than its limit is throttled (slowed down). A container using more memory than its limit is killed (`OOMKilled`) and restarted.

CPU is measured in millicores (`250m` is a quarter of a core), memory in bytes (`128Mi`). The pod's **QoS class** follows from them: `Guaranteed` when requests equal limits, `Burstable` when requests are lower, `BestEffort` when none are set. When a node runs short of memory, Kubernetes evicts `BestEffort` pods first. Here every container sets both, with requests below limits, so everything is `Burstable`.

**Defined in** `charts/service/values.yaml` (the defaults for all five services: requests `25m` / `32Mi`, limits `250m` / `128Mi`) and, for the platform pieces, in their own manifests and values files. All together they keep the cluster inside the 6 GB budget.

```bash
kubectl --context kind-order-saga get pod -n shop -l app.kubernetes.io/name=orders -o jsonpath='{.items[0].spec.containers[0].resources}{"\n"}'
```

You should see `{"limits":{"cpu":"250m","memory":"128Mi"},"requests":{"cpu":"25m","memory":"32Mi"}}`.

```bash
kubectl --context kind-order-saga get pod -n shop -l app.kubernetes.io/name=orders -o jsonpath='{.items[0].status.qosClass}{"\n"}'
```

You should see `Burstable`.

```bash
kubectl --context kind-order-saga describe node order-saga-worker | grep -A 6 "Allocated resources"
```

You should see how much CPU and memory of this node the scheduler has already handed out as requests, as a total and as a percentage. This is the number that decides whether a new pod fits.

`kubectl top pods` (actual usage) does not work yet:

```bash
kubectl --context kind-order-saga top pods -n shop
```

You should see `error: Metrics API not available`. Usage numbers come from the metrics-server, which is not installed; metrics arrive on day 3. Until then you can see what was asked for, but not what is used.

## 10. What is deliberately missing, and what differs on GKE

Not yet in the cluster, on purpose:

- **NetworkPolicies.** Today any pod can talk to any other pod, including from `shop` straight to Postgres in `platform`. Day 4 restricts that.
- **Pod Security Admission labels and RBAC tightening.** The containers already run non-root with a read-only filesystem, but the namespaces do not enforce it yet, and the pods still get the default API credentials mounted. Day 4.
- **Observability.** No metrics-server, no Prometheus, no dashboards, no tracing. Day 3.
- **GitOps.** You deploy with `make local-up` and Helm from your terminal. Argo CD or Flux that watches Git and applies it comes later.

The security context is already in the chart because it is cheap to build in and expensive to retrofit. One detail you will meet: the Dockerfile ends with `USER 65532:65532`, a number, not the name `nonroot`. With `runAsNonRoot: true` the kubelet must be able to prove the user is not root, and it cannot map a name to a number inside the image, so a name would be refused.

What changes on GKE:

| Here (kind) | On GKE |
|---|---|
| Nodes are Docker containers | Nodes are Compute Engine VMs, managed by GKE |
| Images are loaded with `kind load docker-image` | Images are pulled from Artifact Registry |
| NodePort 30080 plus a port mapping | A load balancer created for the Gateway |
| `EnvoyProxy` and the Envoy Gateway install | GKE's own Gateway controller; Gateway and HTTPRoute stay the same, `gatewayClassName` changes |
| Postgres StatefulSet on a local-path disk | Cloud SQL, or a StatefulSet on a Persistent Disk |
| Secret created by a script | Secret Manager or similar, synced into the cluster |
| `kubectl top` missing until day 3 | Metrics are built in |

## Going further

The ADRs explain why each piece is the way it is: [0005](adr/0005-kind-local-cluster.md) (kind), [0006](adr/0006-generic-helm-chart.md) (the chart) and [0007](adr/0007-gateway-api-envoy-gateway.md) (the gateway). When you finish this tour and the Day 2 checkpoint in the README, you should be able to defend them.
