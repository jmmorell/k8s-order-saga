#!/usr/bin/env bash
# Offline test of charts/service, charts/saga-observability and deploy/argocd/apps: lint, schema
# validation and a few structural assertions on the rendered manifests. Needs helm, kubeconform
# and ruby (YAML parsing), no cluster.
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
  # No ServiceAccount token, on the ServiceAccount and on the pod (ADR 0009).
  sa=$(render "$svc" --show-only templates/serviceaccount.yaml)
  grep -qx 'automountServiceAccountToken: false' <<<"$sa" ||
    fail "$svc: ServiceAccount lacks automountServiceAccountToken: false"
  deploy=$(render "$svc" --show-only templates/deployment.yaml)
  grep -qx '      automountServiceAccountToken: false' <<<"$deploy" ||
    fail "$svc: pod spec lacks automountServiceAccountToken: false"
  # The pod-level securityContext: the block at the pod spec's indentation (6 spaces).
  podsc=$(awk '/^      securityContext:$/ {f=1; next} f && /^        / {print; next} f {exit}' <<<"$deploy")
  for s in "runAsNonRoot: true" "runAsUser: 65532" "runAsGroup: 65532" "seccompProfile:" "type: RuntimeDefault"; do
    grep -qF -- "$s" <<<"$podsc" || fail "$svc: pod securityContext lacks '$s'"
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

# deploy/argocd/apps: the app-of-apps chart (ADR 0010). One Argo CD Application per workload
# piece, every one pointing at the same repo and revision, with automated sync, prune and
# self-heal, and none of them creating a namespace (local-up.sh owns those).
echo "== argocd apps"
apps=deploy/argocd/apps
repo=https://github.com/jmmorell/k8s-order-saga.git
helm lint --strict "$apps"
out=$(helm template apps "$apps" --namespace argocd)
# No Application schema offline; the structure is asserted below.
kubeconform -strict -summary -skip Application <<<"$out"

# Prints one line per Application: name, namespace, repo, revision, path, release, destination
# namespace, value files, prune, selfHeal, CreateNamespace option.
app_table() {
  ruby -ryaml -e '
    YAML.load_stream($stdin.read).each do |d|
      next unless d.is_a?(Hash) && d["kind"] == "Application"
      s = d["spec"]; src = s["source"]; h = src["helm"] || {}
      auto = (s["syncPolicy"] || {})["automated"] || {}
      opts = (s["syncPolicy"] || {})["syncOptions"] || []
      files = (h["valueFiles"] || []).join(",")
      puts [d["metadata"]["name"], d["metadata"]["namespace"] || "-", src["repoURL"],
            src["targetRevision"], src["path"], h["releaseName"] || "-",
            s["destination"]["namespace"] || "-", files.empty? ? "-" : files,
            auto["prune"] == true, auto["selfHeal"] == true,
            opts.any? { |o| o.start_with?("CreateNamespace=true") }].join(" ")
    end'
}

table=$(app_table <<<"$out")
[ "$(wc -l <<<"$table")" -eq 7 ] || fail "argocd apps: expected 7 Applications, got:"$'\n'"$table"
# expect_app <name> <path> <release> <destination namespace> <value files>
expect_app() {
  local want="$1 argocd $repo main $2 $3 $4 $5 true true false"
  grep -qxF -- "$want" <<<"$table" || fail "argocd apps: no Application '$want' in:"$'\n'"$table"
}
for svc in $services; do
  expect_app "svc-$svc" charts/service "$svc" shop "../../deploy/local/values/$svc.yaml"
done
expect_app saga-observability charts/saga-observability saga-observability observability -
expect_app policies deploy/local/policies - - -
# Every value file an Application names exists, relative to its chart.
for f in $(awk '$8 != "-" {print $5 "/" $8}' <<<"$table" | tr ',' '\n'); do
  [ -f "$f" ] || fail "argocd apps: value file $f does not exist"
done
# The revision value reaches every Application.
rev=$(helm template apps "$apps" --namespace argocd --set revision=some/branch | app_table | awk '{print $4}' | sort -u)
[ "$rev" = some/branch ] || fail "argocd apps: revision=some/branch rendered targetRevision '$rev'"

# deploy/argocd/root.yaml: the root Application that local-up.sh applies.
root=$(app_table <deploy/argocd/root.yaml)
grep -qxF "root argocd $repo main deploy/argocd/apps - argocd - true true false" <<<"$root" ||
  fail "argocd root: unexpected root Application: $root"
# local-up.sh overrides the revision: the root's targetRevision and the apps chart's value.
grep -qxF '          value: main' deploy/argocd/root.yaml ||
  fail "argocd root: no helm parameter revision=main"

echo "CHART OK"
