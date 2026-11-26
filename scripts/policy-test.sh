#!/usr/bin/env bash
# Checks the security policies of the live kind cluster: each namespace enforces its Pod Security
# level, privileged pods are rejected in shop and platform, no running pod violates its
# namespace's level, and the NetworkPolicies allow and deny the flows of the traffic matrix
# (ADR 0009), probed from short-lived curl pods. Prints POLICY OK.
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=../deploy/local/versions.env
source deploy/local/versions.env

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
PSA_VERSION=v1.37

echo "== Pod Security levels"
for entry in "${levels[@]}"; do
  ns=${entry%%=*} level=${entry#*=}
  got=$("${K[@]}" get ns "$ns" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null || true)
  [ "$got" = "$level" ] || fail "namespace $ns enforces '${got:-nothing}', want '$level'"
  ver=$("${K[@]}" get ns "$ns" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce-version}' 2>/dev/null || true)
  [ "$ver" = "$PSA_VERSION" ] || fail "namespace $ns enforce-version is '${ver:-unset}', want '$PSA_VERSION'"
  echo "$ns: $level ($ver)"
done

echo "== privileged pods are rejected"
for ns in shop platform; do
  if out=$("${K[@]}" -n "$ns" run policy-test-privileged --image=busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec --restart=Never \
    --dry-run=server --overrides='{"spec":{"containers":[{"name":"policy-test-privileged","image":"busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec","securityContext":{"privileged":true}}]}}' \
    -- true 2>&1); then
    fail "$ns: a privileged pod was admitted"
  fi
  grep -qF 'violates PodSecurity "restricted' <<<"$out" || fail "$ns: unexpected rejection: $out"
  echo "$ns: rejected"
done

# psa_warnings <namespace> <level>: the Pod Security warnings of a server-side dry run that sets
# the namespace's enforce level. PSA evaluates existing pods only when a label value changes, and
# relabelling to the level a namespace already has changes nothing, so the dry run also moves
# enforce-version to "latest"; that is what makes the API server check the pods. Fails only if
# the dry run itself fails.
psa_warnings() {
  local out
  out=$("${K[@]}" label ns "$1" "pod-security.kubernetes.io/enforce=$2" \
    pod-security.kubernetes.io/enforce-version=latest --overwrite --dry-run=server 2>&1) ||
    fail "$1: the label dry-run failed:"$'\n'"$out"
  grep -i 'warning' <<<"$out" || true
}

echo "== existing pods satisfy their namespace's level"
for entry in "${levels[@]}"; do
  ns=${entry%%=*} level=${entry#*=}
  warnings=$(psa_warnings "$ns" "$level")
  [ -z "$warnings" ] || fail "$ns: pods violate '$level':"$'\n'"$warnings"
  echo "$ns: no violations at $level"
done
# The check must be able to fail: observability runs pods that are not restricted-clean.
warnings=$(psa_warnings observability restricted)
[ -n "$warnings" ] || fail "the Pod Security check saw no warning for observability at restricted; it cannot fail"
echo "observability at restricted: warnings seen, so the check can fail"

# Each probe is a pod that impersonates a sender through its app.kubernetes.io/name label (or has
# no label at all) and connects with curl. "allow" needs the TCP connection to succeed (and an
# HTTP URL to answer); "deny" needs curl to time out before connecting.
# name|namespace|label (empty: none)|url|allow or deny
probes=(
  "loadgen-orders|shop|loadgen|http://orders.shop:8080/healthz|allow"
  "notifications-mailpit|shop|notifications|telnet://mailpit.platform:1025|allow"
  "unlabelled-orders|shop||http://orders.shop:8080/healthz|deny"
  "unlabelled-postgres|shop||telnet://postgres.platform:5432|deny"
  "loadgen-postgres|shop|loadgen|telnet://postgres.platform:5432|deny"
  "platform-orders|platform||http://orders.shop:8080/healthz|deny"
  "notifications-internet|shop|notifications|https://example.com|deny"
)

created=()
cleanup() {
  local p
  for p in "${created[@]+"${created[@]}"}"; do
    "${K[@]}" -n "${p%%/*}" delete pod "${p#*/}" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# probe_pod <name> <namespace> <label>: a pod that passes Pod Security restricted and sleeps, so
# curl runs through kubectl exec once the pod is Ready.
probe_pod() {
  local name=$1 ns=$2 label=$3 labels="{}"
  [ -n "$label" ] && labels="{app.kubernetes.io/name: $label}"
  "${K[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $ns
  labels: $labels
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  securityContext:
    runAsNonRoot: true
    runAsUser: 100
    runAsGroup: 101
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: curl
      image: $CURL_IMAGE
      command: ["sleep", "300"]
      securityContext:
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      resources:
        requests: {cpu: 10m, memory: 16Mi}
        limits: {cpu: 100m, memory: 32Mi}
EOF
}

echo "== network probes"
for p in "${probes[@]}"; do
  IFS='|' read -r name ns label _ _ <<<"$p"
  probe_pod "policy-probe-$name" "$ns" "$label"
  created+=("$ns/policy-probe-$name")
done
for p in "${created[@]}"; do
  "${K[@]}" -n "${p%%/*}" wait --for=condition=Ready "pod/${p#*/}" --timeout=120s >/dev/null ||
    fail "probe pod $p is not Ready"
done
# kindnet adds a pod to its policy sets shortly after the pod starts; packets sent before that
# are not filtered (ADR 0009). Give it a moment so the probes see the policies.
sleep 5

failed=0
for p in "${probes[@]}"; do
  IFS='|' read -r name ns label url want <<<"$p"
  # Prints "<curl exit code> <time to connect>"; stdin is closed, so a telnet:// probe ends at once.
  res=$("${K[@]}" -n "$ns" exec "policy-probe-$name" -- sh -c \
    "curl -sS -o /dev/null --max-time 3 -w '%{time_connect}' '$url' </dev/null 2>/dev/null; echo \" \$?\"" 2>&1) ||
    fail "probe $name: kubectl exec failed: $res"
  read -r connect code <<<"$res"
  if [ "$connect" != "0.000000" ] && { [ "$code" = 0 ] || [[ $url == telnet://* ]]; }; then
    got=allow
  elif [ "$code" = 28 ] && [ "$connect" = "0.000000" ]; then
    got=deny
  else
    got="unexpected (curl exit $code, connect $connect)"
  fi
  if [ "$got" = "$want" ]; then
    echo "ok   $name: ${label:-unlabelled} in $ns -> $url: $got"
  else
    echo "FAIL $name: ${label:-unlabelled} in $ns -> $url: want $want, got $got (curl exit $code)" >&2
    failed=$((failed + 1))
  fi
done
[ "$failed" -eq 0 ] || fail "$failed of ${#probes[@]} network probes did not match"

echo "POLICY OK"
