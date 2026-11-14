#!/usr/bin/env bash
# Creates the kind cluster and installs NATS, Postgres, Mailpit, Envoy Gateway and the five
# services. Safe to re-run. 'local-up.sh reload' only rebuilds and restarts the services.
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
ensure_secrets() {
  echo "==> ensuring postgres secret"
  local password
  if "${K[@]}" -n platform get secret postgres >/dev/null 2>&1; then
    if ! "${K[@]}" -n shop get secret postgres >/dev/null 2>&1; then
      password=$("${K[@]}" -n platform get secret postgres -o jsonpath='{.data.password}' | base64 --decode)
      "${K[@]}" -n shop create secret generic postgres --from-literal=password="$password" >/dev/null
    fi
    return
  fi
  password=$(openssl rand -hex 16)
  "${K[@]}" -n platform create secret generic postgres --from-literal=password="$password" >/dev/null
  "${K[@]}" -n shop create secret generic postgres --from-literal=password="$password" >/dev/null
}

install_platform() {
  echo "==> installing postgres and mailpit"
  "${K[@]}" apply -f deploy/local/platform/postgres.yaml -f deploy/local/platform/mailpit.yaml
  echo "==> installing nats chart $NATS_CHART_VERSION"
  helm repo add nats https://nats-io.github.io/k8s/helm/charts/ --force-update >/dev/null
  helm upgrade --install nats nats/nats --version "$NATS_CHART_VERSION" \
    --kube-context "$CTX" -n platform -f deploy/local/values/nats.yaml
}

wait_platform() {
  echo "==> waiting for platform pods"
  "${K[@]}" -n platform rollout status statefulset/postgres --timeout=180s
  "${K[@]}" -n platform rollout status statefulset/nats --timeout=180s
  "${K[@]}" -n platform rollout status deployment/mailpit --timeout=180s
}

install_gateway() {
  echo "==> installing envoy gateway $ENVOY_GATEWAY_VERSION"
  helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version "$ENVOY_GATEWAY_VERSION" \
    --kube-context "$CTX" -n envoy-gateway-system --create-namespace
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

# Installs all five releases at once; the services wait for their dependencies themselves.
install_services() {
  echo "==> installing services"
  local svc
  for svc in "${SERVICES[@]}"; do
    helm upgrade --install "$svc" charts/service --kube-context "$CTX" -n shop \
      -f "deploy/local/values/$svc.yaml" --set image.repository="k8s-order-saga/$svc"
  done
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
install_gateway
build_and_load_images
install_services
wait_services
echo "==> local cluster ready (context $CTX)"
echo "orders: http://localhost:8080/orders"
echo "next:   make local-smoke | make local-status | make local-dlq | make local-reload"
