#!/usr/bin/env bash
# Checks the security policies of the live kind cluster: each namespace enforces its Pod Security
# level, privileged pods are rejected in shop and platform, and no running pod violates its
# namespace's level. Prints POLICY OK.
set -euo pipefail

K=(kubectl --context kind-order-saga)

fail() { echo "FAIL: $*" >&2; exit 1; }

# namespace -> expected enforce level (ADR 0009).
levels=(
  shop=restricted
  platform=restricted
  observability=baseline
  envoy-gateway-system=baseline
  argocd=baseline
)

echo "== Pod Security levels"
for entry in "${levels[@]}"; do
  ns=${entry%%=*} level=${entry#*=}
  got=$("${K[@]}" get ns "$ns" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null || true)
  [ "$got" = "$level" ] || fail "namespace $ns enforces '${got:-nothing}', want '$level'"
  echo "$ns: $level"
done

echo "== privileged pods are rejected"
for ns in shop platform; do
  if out=$("${K[@]}" -n "$ns" run policy-test-privileged --image=busybox:1.37 --restart=Never \
    --dry-run=server --overrides='{"spec":{"containers":[{"name":"policy-test-privileged","image":"busybox:1.37","securityContext":{"privileged":true}}]}}' \
    -- true 2>&1); then
    fail "$ns: a privileged pod was admitted"
  fi
  grep -qF 'violates PodSecurity "restricted' <<<"$out" || fail "$ns: unexpected rejection: $out"
  echo "$ns: rejected"
done

echo "== existing pods satisfy their namespace's level"
for entry in "${levels[@]}"; do
  ns=${entry%%=*} level=${entry#*=}
  out=$("${K[@]}" label ns "$ns" "pod-security.kubernetes.io/enforce=$level" --overwrite --dry-run=server 2>&1)
  if grep -qi 'warning' <<<"$out"; then
    fail "$ns: pods violate '$level':"$'\n'"$out"
  fi
  echo "$ns: no violations at $level"
done

echo "POLICY OK"
