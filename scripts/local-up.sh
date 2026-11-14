#!/usr/bin/env bash
# Creates the kind cluster and installs NATS, Postgres and Mailpit. Safe to re-run.
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=../deploy/local/versions.env
source deploy/local/versions.env

CLUSTER=order-saga
CTX="kind-${CLUSTER}"
K=(kubectl --context "$CTX")

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER"
}

check_prereqs() {
  echo "==> checking prerequisites"
  local tool
  for tool in kind kubectl helm docker openssl; do
    command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
  done
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

check_prereqs
create_cluster
apply_namespaces
ensure_secrets
install_platform
wait_platform
echo "==> local cluster ready (context $CTX)"
