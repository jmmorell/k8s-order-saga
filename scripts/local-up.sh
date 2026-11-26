#!/usr/bin/env bash
# Creates the kind cluster and installs NATS, Postgres, Mailpit, the observability stack, Envoy
# Gateway and the five services, then the NetworkPolicies. Safe to re-run. 'local-up.sh reload'
# only rebuilds and restarts the services.
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=../deploy/local/versions.env
source deploy/local/versions.env

CLUSTER=order-saga
CTX="kind-${CLUSTER}"
K=(kubectl --context "$CTX")
SERVICES=(orders inventory payments notifications loadgen)

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER"
}

check_prereqs() {
  echo "==> checking prerequisites"
  local tool
  for tool in kind kubectl helm docker openssl; do
    command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
  done
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

  # Needs the PrometheusRule CRD from kube-prometheus-stack, installed above.
  echo "==> installing saga-observability chart (dashboard, SLO, alerts)"
  helm upgrade --install saga-observability charts/saga-observability \
    --kube-context "$CTX" -n observability
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

build_and_load_images() {
  echo "==> building and loading service images"
  local svc
  for svc in "${SERVICES[@]}"; do
    docker build -q --build-arg SERVICE="$svc" -t "k8s-order-saga/$svc:dev" . >/dev/null
  done
  local images=()
  for svc in "${SERVICES[@]}"; do images+=("k8s-order-saga/$svc:dev"); done
  kind load docker-image --name "$CLUSTER" "${images[@]}"
}

# Installs all five releases at once. A service opens its HTTP port only after Postgres and NATS
# answer and waits for both without giving up, so the chart's startup probe gives it up to
# 2 minutes, after which the kubelet restarts it; liveness never depends on them.
install_services() {
  echo "==> installing services"
  local svc
  for svc in "${SERVICES[@]}"; do
    helm upgrade --install "$svc" charts/service --kube-context "$CTX" -n shop \
      -f "deploy/local/values/$svc.yaml"
  done
}

# Default-deny NetworkPolicies with explicit allows (ADR 0009), for shop, platform, observability
# and argocd. Applied once everything is installed; Argo CD takes them over later.
apply_policies() {
  echo "==> applying network policies"
  "${K[@]}" apply -f deploy/local/policies/
}

wait_services() {
  echo "==> waiting for services"
  local svc
  for svc in "${SERVICES[@]}"; do
    "${K[@]}" -n shop rollout status "deployment/$svc" --timeout=180s
  done
}

if [ "${1:-}" = reload ]; then
  build_and_load_images
  for svc in "${SERVICES[@]}"; do
    "${K[@]}" -n shop rollout restart "deployment/$svc"
  done
  wait_services
  exit 0
fi

check_prereqs
create_cluster
apply_namespaces
ensure_secrets
install_platform
wait_platform
install_observability
install_gateway
build_and_load_images
install_services
apply_policies
wait_services
echo "==> local cluster ready (context $CTX)"
echo "orders: http://localhost:8080/orders"
echo "next:   make local-smoke | make local-status | make local-dlq | make local-reload"
echo "        make grafana | make prom | make alerts | make obs-check"
