#!/usr/bin/env bash
# Creates the kind cluster and installs NATS, Postgres, Mailpit, the observability stack, Envoy
# Gateway and Argo CD, then hands the workload (the five services, saga-observability and the
# NetworkPolicies) to Argo CD's app-of-apps (ADR 0010). ARGOCD_REVISION (default main) is the Git
# revision Argo CD tracks. Safe to re-run. 'local-up.sh reload' only rebuilds and restarts the
# services.
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=../deploy/local/versions.env
source deploy/local/versions.env

CLUSTER=order-saga
CTX="kind-${CLUSTER}"
K=(kubectl --context "$CTX")
SERVICES=(orders inventory payments notifications loadgen)
ARGOCD_REVISION=${ARGOCD_REVISION:-main}

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER"
}

check_prereqs() {
  echo "==> checking prerequisites"
  local tool
  for tool in kind kubectl helm docker openssl; do
    command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
  done
  # It is substituted into deploy/argocd/root.yaml with sed.
  if ! [[ $ARGOCD_REVISION =~ ^[A-Za-z0-9._/-]+$ ]]; then
    echo "ARGOCD_REVISION '$ARGOCD_REVISION' is not a plain branch, tag or commit name" >&2
    exit 1
  fi
  if [ "$(grep -c "image: $KIND_NODE_IMAGE\$" deploy/local/kind-cluster.yaml)" -ne 3 ]; then
    echo "kind-cluster.yaml node images do not match KIND_NODE_IMAGE in versions.env" >&2
    exit 1
  fi
  docker info >/dev/null 2>&1 || { echo "docker is not running" >&2; exit 1; }
  if ! cluster_exists && lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "host port 8080 is in use (is Docker Compose running? try 'make down')" >&2
    exit 1
  fi
}

create_cluster() {
  if cluster_exists; then
    echo "==> cluster $CLUSTER already exists"
    return
  fi
  echo "==> creating kind cluster $CLUSTER"
  kind create cluster --config deploy/local/kind-cluster.yaml --name "$CLUSTER"
}

# Clusters from day 2 and 3 installed the services and saga-observability with Helm. Argo CD
# applies the same objects without Helm, so the old releases would be left behind, half owned by
# both. Rather than adopting them, ask for a fresh cluster once.
check_no_helm_workload() {
  local releases
  releases=$(helm list --kube-context "$CTX" -A -q --filter \
    "^($(IFS='|'; echo "${SERVICES[*]}")|saga-observability)\$" 2>/dev/null || true)
  if [ -n "$releases" ]; then
    echo "this cluster still has the Helm releases of an older local-up: $(tr '\n' ' ' <<<"$releases")" >&2
    echo "Argo CD now deploys them; recreate the cluster once: make local-down && make local-up" >&2
    exit 1
  fi
}

apply_namespaces() {
  echo "==> applying namespaces"
  "${K[@]}" apply -f deploy/local/platform/namespaces.yaml
}

# Creates Secret postgres (key password) in platform and shop; never prints the password.
ensure_postgres_secret() {
  echo "==> ensuring postgres secret"
  local password
  local have_platform=false have_shop=false
  "${K[@]}" -n platform get secret postgres >/dev/null 2>&1 && have_platform=true
  "${K[@]}" -n shop get secret postgres >/dev/null 2>&1 && have_shop=true
  if $have_platform && $have_shop; then
    return
  fi
  # One of the two may survive a half-finished run: copy it so both always hold the same password.
  if $have_platform; then
    password=$("${K[@]}" -n platform get secret postgres -o jsonpath='{.data.password}' | base64 --decode)
    "${K[@]}" -n shop create secret generic postgres --from-literal=password="$password" >/dev/null
  elif $have_shop; then
    password=$("${K[@]}" -n shop get secret postgres -o jsonpath='{.data.password}' | base64 --decode)
    "${K[@]}" -n platform create secret generic postgres --from-literal=password="$password" >/dev/null
  else
    password=$(openssl rand -hex 16)
    "${K[@]}" -n platform create secret generic postgres --from-literal=password="$password" >/dev/null
    "${K[@]}" -n shop create secret generic postgres --from-literal=password="$password" >/dev/null
  fi
}

# Creates Secret grafana-admin (keys admin-user, admin-password) in observability, only if it is
# absent; never prints the password.
ensure_grafana_secret() {
  echo "==> ensuring grafana-admin secret"
  "${K[@]}" -n observability get secret grafana-admin >/dev/null 2>&1 && return
  "${K[@]}" -n observability create secret generic grafana-admin \
    --from-literal=admin-user=admin --from-literal=admin-password="$(openssl rand -hex 16)" >/dev/null
}

ensure_secrets() {
  ensure_postgres_secret
  ensure_grafana_secret
}

# The NATS PodMonitor needs the Prometheus Operator CRDs, which kube-prometheus-stack installs
# after the platform. Until they exist NATS is installed without it, and install_observability
# installs NATS again once they do.
install_nats() {
  local podmonitor=false
  "${K[@]}" get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1 && podmonitor=true
  echo "==> installing nats chart $NATS_CHART_VERSION (PodMonitor: $podmonitor)"
  helm repo add nats https://nats-io.github.io/k8s/helm/charts/ --force-update >/dev/null
  helm upgrade --install nats nats/nats --version "$NATS_CHART_VERSION" \
    --kube-context "$CTX" -n platform -f deploy/local/values/nats.yaml \
    --set promExporter.podMonitor.enabled="$podmonitor"
}

install_platform() {
  echo "==> installing postgres and mailpit"
  # serviceName is immutable: a cluster from before postgres-headless existed needs the
  # StatefulSet recreated. --cascade=orphan keeps the pod and the PVC, so the data survives.
  if [ "$("${K[@]}" -n platform get statefulset postgres -o jsonpath='{.spec.serviceName}' 2>/dev/null)" = postgres ]; then
    echo "==> recreating the postgres StatefulSet (serviceName changed); pod and data are kept"
    "${K[@]}" -n platform delete statefulset postgres --cascade=orphan
  fi
  "${K[@]}" apply -f deploy/local/platform/postgres.yaml -f deploy/local/platform/mailpit.yaml
  install_nats
}

wait_platform() {
  echo "==> waiting for platform pods"
  "${K[@]}" -n platform rollout status statefulset/postgres --timeout=180s
  "${K[@]}" -n platform rollout status statefulset/nats --timeout=180s
  "${K[@]}" -n platform rollout status deployment/mailpit --timeout=180s
}

# Runs before the services are installed, so their first spans and metrics have somewhere to go.
# The namespace itself comes from apply_namespaces (ensure_grafana_secret needs it earlier).
install_observability() {
  echo "==> installing metrics-server chart $METRICS_SERVER_CHART_VERSION"
  helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update >/dev/null
  helm upgrade --install metrics-server metrics-server/metrics-server \
    --version "$METRICS_SERVER_CHART_VERSION" --kube-context "$CTX" -n kube-system \
    -f deploy/local/values/metrics-server.yaml
  echo "==> installing kube-prometheus-stack chart $KUBE_PROMETHEUS_STACK_VERSION"
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update >/dev/null
  helm upgrade --install kps prometheus-community/kube-prometheus-stack \
    --version "$KUBE_PROMETHEUS_STACK_VERSION" --kube-context "$CTX" -n observability \
    -f deploy/local/values/kube-prometheus-stack.yaml
  if ! "${K[@]}" -n platform get podmonitor nats >/dev/null 2>&1; then
    install_nats
  fi
  echo "==> installing tempo chart $TEMPO_CHART_VERSION"
  helm repo add grafana-community https://grafana-community.github.io/helm-charts --force-update >/dev/null
  helm upgrade --install tempo grafana-community/tempo --version "$TEMPO_CHART_VERSION" \
    --kube-context "$CTX" -n observability -f deploy/local/values/tempo.yaml
  echo "==> installing loki chart $LOKI_CHART_VERSION"
  helm upgrade --install loki grafana-community/loki --version "$LOKI_CHART_VERSION" \
    --kube-context "$CTX" -n observability -f deploy/local/values/loki.yaml
  echo "==> installing opentelemetry-collector chart $OTEL_COLLECTOR_CHART_VERSION"
  helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts --force-update >/dev/null
  helm upgrade --install otel-collector open-telemetry/opentelemetry-collector \
    --version "$OTEL_COLLECTOR_CHART_VERSION" --kube-context "$CTX" -n observability \
    -f deploy/local/values/otel-collector.yaml

  echo "==> waiting for the observability stack"
  "${K[@]}" -n kube-system rollout status deployment/metrics-server --timeout=180s
  local d
  for d in kps-operator kps-kube-state-metrics kps-grafana otel-collector; do
    "${K[@]}" -n observability rollout status "deployment/$d" --timeout=300s
  done
  "${K[@]}" -n observability rollout status statefulset/tempo --timeout=180s
  "${K[@]}" -n observability rollout status statefulset/loki --timeout=180s
  # The operator creates these StatefulSets asynchronously; wait on the custom resources instead.
  "${K[@]}" -n observability wait --for=condition=Available prometheus/kps-prometheus --timeout=300s
  "${K[@]}" -n observability wait --for=condition=Available alertmanager/kps-alertmanager --timeout=300s
  "${K[@]}" -n platform rollout status statefulset/nats --timeout=180s
  # charts/saga-observability (dashboard, SLO, alerts) is Argo CD's; it needs the PrometheusRule
  # CRD installed above.
}

install_gateway() {
  echo "==> installing envoy gateway $ENVOY_GATEWAY_VERSION"
  helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version "$ENVOY_GATEWAY_VERSION" \
    --kube-context "$CTX" -n envoy-gateway-system --create-namespace \
    -f deploy/local/values/envoy-gateway.yaml
  "${K[@]}" -n envoy-gateway-system rollout status deployment/envoy-gateway --timeout=180s
  "${K[@]}" apply -f deploy/local/gateway/envoyproxy.yaml -f deploy/local/gateway/gatewayclass.yaml \
    -f deploy/local/gateway/gateway.yaml -f deploy/local/gateway/httproute.yaml
  "${K[@]}" -n shop wait --for=condition=Programmed gateway/shop --timeout=180s
}

# Prints <image.repository>:<image.tag> from deploy/local/values/<service>.yaml (the tag unquoted),
# the reference the chart deploys, so the cluster never pulls a tag built here from the registry.
image_ref() {
  awk '/^image:[[:space:]]*$/ {f=1; next} f && /^[^[:space:]#]/ {f=0}
    f && $1 == "repository:" {r=$2} f && $1 == "tag:" {t=$2}
    END {gsub(/"/, "", r); gsub(/"/, "", t); if (r == "" || t == "") exit 1; print r ":" t}' \
    "deploy/local/values/$1.yaml" || { echo "deploy/local/values/$1.yaml: no image.repository or image.tag" >&2; exit 1; }
}

# Services whose values tag is dev, the tag of a local build. Any other tag is a release that
# CI pushed to GHCR; the nodes pull it, and local code is never loaded under a release tag.
dev_services() {
  local svc ref
  for svc in "${SERVICES[@]}"; do
    ref=$(image_ref "$svc")
    if [ "${ref##*:}" = dev ]; then
      echo "$svc"
    else
      echo "    $svc: release tag ${ref##*:}, pulled from the registry, not built" >&2
    fi
  done
}

# Builds each dev-tagged service for the host's architecture (kind load cannot load a multi-arch
# image). Runs before Argo CD syncs, so the dev tag is on the nodes when the pods start.
build_and_load_images() {
  echo "==> building and loading service images"
  local svc ref devs images=()
  devs=$(dev_services)
  for svc in $devs; do
    ref=$(image_ref "$svc")
    docker build -q --build-arg SERVICE="$svc" -t "$ref" . >/dev/null
    images+=("$ref")
  done
  if [ "${#images[@]}" -eq 0 ]; then
    echo "    no service uses the dev tag; nothing to build"
    return
  fi
  echo "    ${images[*]}"
  kind load docker-image --name "$CLUSTER" "${images[@]}"
}

# Argo CD itself is bootstrap, installed here with Helm; the workload is Argo CD's (ADR 0010).
# Its UI is reached with make argocd (port-forward); the Service stays ClusterIP.
install_argocd() {
  echo "==> installing argo-cd chart $ARGOCD_CHART_VERSION"
  helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
  helm upgrade --install argocd argo/argo-cd --version "$ARGOCD_CHART_VERSION" \
    --kube-context "$CTX" -n argocd -f deploy/local/values/argo-cd.yaml
  local d
  for d in argocd-redis argocd-repo-server argocd-server; do
    "${K[@]}" -n argocd rollout status "deployment/$d" --timeout=180s
  done
  "${K[@]}" -n argocd rollout status statefulset/argocd-application-controller --timeout=180s
}

# The root Application, tracking ARGOCD_REVISION: its own targetRevision and the revision value it
# passes to deploy/argocd/apps, so every child Application tracks the same revision.
apply_root_app() {
  echo "==> applying the root Application (revision $ARGOCD_REVISION)"
  sed -E "s#^( *(targetRevision|value): )main\$#\1${ARGOCD_REVISION}#" deploy/argocd/root.yaml |
    "${K[@]}" apply -f -
}

# Waits until root and the Applications it renders are all Synced and Healthy. A service opens its
# HTTP port only after Postgres and NATS answer, and its startup probe gives it up to 2 minutes.
wait_apps() {
  echo "==> waiting for the Argo CD Applications (Synced and Healthy)"
  local want=$((1 + ${#SERVICES[@]} + 2)) deadline=$((SECONDS + 300)) status ok=0
  while :; do
    status=$("${K[@]}" -n argocd get applications.argoproj.io \
      -o jsonpath='{range .items[*]}{.metadata.name} {.status.sync.status} {.status.health.status}{"\n"}{end}')
    # Three polls in a row: right after a sync an Application can read Synced before the
    # controller has compared it again.
    if [ "$(grep -c ' Synced Healthy$' <<<"$status")" -eq "$want" ] &&
      [ "$(grep -c . <<<"$status")" -eq "$want" ]; then
      ok=$((ok + 1))
    else
      ok=0
    fi
    if [ "$ok" -ge 3 ]; then
      echo "    $want Applications Synced and Healthy"
      return
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "Applications not Synced and Healthy after 300 s:" >&2
      echo "$status" >&2
      exit 1
    fi
    sleep 5
  done
}

if [ "${1:-}" = reload ]; then
  # A restart only changes the pod template's restartedAt annotation, which is not in Git, so
  # Argo CD's self-heal leaves it alone (ADR 0010).
  if [ -z "$(dev_services 2>/dev/null)" ]; then
    dev_services >/dev/null # names each service and its release tag on stderr
    echo "no service uses the dev tag, so there is no local code to load. To work on local code: on a branch run" >&2
    echo "  scripts/bump-image-tag.sh dev   # sets tag: \"dev\" in the values files; commit and push the branch" >&2
    echo "  ARGOCD_REVISION=<branch> make local-up   # Argo CD tracks the branch; then make local-reload" >&2
    exit 1
  fi
  build_and_load_images
  svcs=$(dev_services 2>/dev/null) # build_and_load_images already named the skipped ones
  for svc in $svcs; do
    "${K[@]}" -n shop rollout restart "deployment/$svc"
  done
  echo "==> waiting for services"
  for svc in $svcs; do
    "${K[@]}" -n shop rollout status "deployment/$svc" --timeout=180s
  done
  exit 0
fi

check_prereqs
create_cluster
check_no_helm_workload
apply_namespaces
ensure_secrets
install_platform
wait_platform
install_observability
install_gateway
build_and_load_images
install_argocd
apply_root_app
wait_apps
echo "==> local cluster ready (context $CTX, Argo CD tracking $ARGOCD_REVISION)"
echo "orders: http://localhost:8080/orders"
echo "next:   make local-smoke | make local-status | make local-dlq | make local-reload"
echo "        make grafana | make prom | make alerts | make obs-check | make argocd"
