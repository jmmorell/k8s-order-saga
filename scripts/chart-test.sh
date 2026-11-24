#!/usr/bin/env bash
# Offline test of charts/service and charts/saga-observability: lint, schema validation and
# a few structural assertions on the rendered manifests. Needs helm and kubeconform, no cluster.
set -euo pipefail

cd "$(dirname "$0")/.."

chart=charts/service
services="orders inventory payments notifications loadgen"

fail() { echo "FAIL: $*" >&2; exit 1; }

render() { # render <service> [extra helm args...]
  local svc=$1; shift
  helm template "$svc" "$chart" --namespace shop -f "deploy/local/values/$svc.yaml" "$@"
}

# expect <service> <fixed string> — the string must appear in the rendered output.
expect() {
  grep -qF -- "$2" <<<"$3" || fail "$1: rendered manifest lacks '$2'"
}

for svc in $services; do
  echo "== $svc"
  helm lint --strict "$chart" -f "deploy/local/values/$svc.yaml"

  out=$(render "$svc")
  kubeconform -strict -summary <<<"$out"

  for s in "readOnlyRootFilesystem: true" "runAsNonRoot: true" \
    "allowPrivilegeEscalation: false" "startupProbe:" "livenessProbe:" "readinessProbe:" \
    "path: /healthz" "path: /readyz" "failureThreshold: 24" "timeoutSeconds: 3" \
    "preStop:" "seconds: 5" "requests:" "limits:" "kind: Deployment" \
    "kind: Service" "kind: ServiceAccount" "- name: HTTP_ADDR"; do
    expect "$svc" "$s" "$out"
  done

  # The local values point every service at the Collector.
  for s in "- name: POD_NAME" "fieldPath: metadata.name" "- name: POD_NAMESPACE" \
    "fieldPath: metadata.namespace" "- name: OTEL_EXPORTER_OTLP_ENDPOINT" \
    'value: "http://otel-collector.observability.svc.cluster.local:4318"' \
    "- name: OTEL_SERVICE_NAME" "value: \"$svc\"" \
    'value: "k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE)"' \
    "- name: OTEL_METRIC_EXPORT_INTERVAL" 'value: "15000"'; do
    expect "$svc" "$s" "$out"
  done
  pod=$(awk '/- name: POD_NAME$/ {print NR; exit}' <<<"$out")
  attrs=$(awk '/- name: OTEL_RESOURCE_ATTRIBUTES/ {print NR; exit}' <<<"$out")
  [ -n "$pod" ] && [ -n "$attrs" ] || fail "$svc: POD_NAME or OTEL_RESOURCE_ATTRIBUTES env entry missing"
  [ "$pod" -lt "$attrs" ] || fail "$svc: POD_NAME must be listed before OTEL_RESOURCE_ATTRIBUTES"
done

# Without otel.endpoint nothing OpenTelemetry-related is rendered.
plain=$(render orders --set otel.endpoint=)
for s in OTEL_ POD_NAME POD_NAMESPACE; do
  if grep -qF -- "$s" <<<"$plain"; then fail "orders: '$s' rendered with an empty otel.endpoint"; fi
done

orders=$(render orders)
expect orders "replicas: 2" "$orders"

pw=$(awk '/- name: POSTGRES_PASSWORD/ {print NR; exit}' <<<"$orders")
db=$(awk '/- name: DATABASE_URL/ {print NR; exit}' <<<"$orders")
[ -n "$pw" ] && [ -n "$db" ] || fail "orders: POSTGRES_PASSWORD or DATABASE_URL env entry missing"
[ "$pw" -lt "$db" ] || fail "orders: POSTGRES_PASSWORD must be listed before DATABASE_URL"
expect orders '$(POSTGRES_PASSWORD)' "$orders"

if helm template orders "$chart" --namespace shop >/dev/null 2>&1; then
  fail "rendering without image.repository should fail"
fi

# charts/saga-observability: dashboards and rules for kube-prometheus-stack.
echo "== saga-observability"
obs=charts/saga-observability
helm lint --strict "$obs"
out=$(helm template saga-observability "$obs" --namespace observability)
# No PrometheusRule schema offline; its content is checked against the rule file below.
kubeconform -strict -summary -skip PrometheusRule <<<"$out"
for s in "kind: PrometheusRule" "kind: ConfigMap" 'grafana_dashboard: "1"' "saga.json: |-"; do
  expect saga-observability "$s" "$out"
done
# The PrometheusRule spec is the rule file unchanged, so promtool tests what ships.
# (Blank lines are ignored: helm's indent leaves a trailing one.)
rule=$(helm template saga-observability "$obs" --namespace observability \
  --show-only templates/prometheusrule.yaml | sed '1,/^spec:$/d; s/^  //; /^$/d')
diff <(sed '/^$/d' "$obs/rules/saga.rules.yaml") <(echo "$rule") >/dev/null ||
  fail "saga-observability: PrometheusRule spec differs from rules/saga.rules.yaml"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["uid"]=="saga" and d["title"]=="Order saga"' \
  "$obs/dashboards/saga.json" || fail "saga-observability: dashboards/saga.json is not the Order saga dashboard"

echo "CHART OK"
