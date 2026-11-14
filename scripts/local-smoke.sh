#!/usr/bin/env bash
# Runs scripts/smoke.sh against the kind cluster: orders through the gateway on
# localhost:8080, Mailpit through a temporary port-forward.
set -euo pipefail

cd "$(dirname "$0")/.."

pf=""
cleanup() { if [ -n "$pf" ]; then kill "$pf" 2>/dev/null || true; fi; }
trap cleanup EXIT

kubectl --context kind-order-saga -n platform port-forward svc/mailpit 18025:8025 >/dev/null 2>&1 &
pf=$!

for _ in $(seq 1 50); do
  curl -fsS -o /dev/null http://localhost:18025/api/v1/info 2>/dev/null && break
  sleep 0.2
done

ORDERS_URL=http://localhost:8080 ORDERS_READY_PATH=/orders/00000000-0000-0000-0000-000000000000 \
  ORDERS_READY_BODY='"error":"not found"' MAILPIT_URL=http://localhost:18025 ./scripts/smoke.sh
