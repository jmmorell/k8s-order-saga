#!/usr/bin/env bash
# Runs every PromQL expression of the saga dashboard and rule file as an instant query against
# the live Prometheus. Fails if a query errors, or if a dashboard or recording-rule query returns
# no data, except for series that are legitimately empty on a healthy cluster. Alert expressions
# only have to evaluate: they return nothing while the alert is not firing.
# Needs some minutes of loadgen traffic (the 5m rates). Prints OBS OK.
set -euo pipefail

cd "$(dirname "$0")/.."
PROM=http://localhost:19090

pf=""
cleanup() { if [ -n "$pf" ]; then kill "$pf" 2>/dev/null || true; fi; }
trap cleanup EXIT

kubectl --context kind-order-saga -n observability port-forward svc/kps-prometheus 19090:9090 >/dev/null 2>&1 &
pf=$!

for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$PROM/-/ready" 2>/dev/null && break
  sleep 0.2
done

python3 - "$PROM" charts/saga-observability/dashboards/saga.json \
  charts/saga-observability/rules/saga.rules.yaml <<'EOF'
import json
import re
import sys
import urllib.parse
import urllib.request

prom, dashboard, rules = sys.argv[1:4]

# Empty on a healthy cluster: nothing dead-lettered, nothing stuck, DLQ stream empty.
ALLOW_EMPTY = ('result="dead_letter"', "saga_orders_stuck_pending", 'stream_name="DLQ"')

checks = []  # (source, expr, must_have_data)

for panel in json.load(open(dashboard))["panels"]:
    for t in panel.get("targets", []):
        # Grafana's $__rate_interval is not PromQL; 5m matches the rules' shortest window.
        expr = t["expr"].replace("$__rate_interval", "5m")
        checks.append((f'dashboard "{panel["title"]}"', expr, True))

# The rule file keeps each expr on one line (see its header), so no YAML parser is needed.
name, kind = None, None
for n, line in enumerate(open(rules), 1):
    m = re.match(r"\s*- (record|alert): (\S+)", line)
    if m:
        kind, name = m.groups()
        continue
    m = re.match(r"\s*expr: (.*)", line)
    if m:
        expr = m.group(1).strip()
        if not expr or expr[0] in "|>":
            sys.exit(f"{rules}:{n}: expr of {name} must be on one line")
        checks.append((f"{kind} {name}", expr, kind == "record"))

failed = 0
for source, expr, must_have_data in checks:
    url = prom + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            body = json.load(r)
    except urllib.error.HTTPError as e:
        body = json.load(e)
    if body.get("status") != "success":
        print(f"ERROR   {source}: {body.get('error')}\n        {expr}")
        failed += 1
        continue
    n = len(body["data"]["result"])
    if n == 0 and must_have_data and not any(a in expr for a in ALLOW_EMPTY):
        print(f"NO DATA {source}\n        {expr}")
        failed += 1
        continue
    print(f"ok      {source}: {n} series")

if failed:
    sys.exit(f"OBS FAILED: {failed} of {len(checks)} queries")
print(f"{len(checks)} queries checked")
print("OBS OK")
EOF
