#!/usr/bin/env bash
# Finds the trace of one order in Tempo (TraceQL on span attribute order.id) and checks that it
# spans orders, inventory, payments and notifications. Prints the trace id and its services.
# Usage: scripts/trace-check.sh <order_id>
set -euo pipefail

order_id=${1:?usage: $0 <order_id>}
TEMPO=http://localhost:13200
REQUIRED="orders inventory payments notifications"

pf=""
cleanup() { if [ -n "$pf" ]; then kill "$pf" 2>/dev/null || true; fi; }
trap cleanup EXIT

kubectl --context kind-order-saga -n observability port-forward svc/tempo 13200:3200 >/dev/null 2>&1 &
pf=$!

for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$TEMPO/ready" 2>/dev/null && break
  sleep 0.2
done

# Prints "<trace id> <service> <service>..." for every trace that matches the order id.
traces_of_order() {
  local now ids id
  now=$(date +%s)
  ids=$(curl -fsS -G "$TEMPO/api/search" \
    --data-urlencode "q={ span.order.id = \"$order_id\" }" \
    --data-urlencode "start=$((now - 3600))" --data-urlencode "end=$((now + 60))" \
    --data-urlencode "limit=20" |
    python3 -c 'import json,sys; [print(t["traceID"]) for t in json.load(sys.stdin).get("traces") or []]')
  for id in $ids; do
    curl -fsS -H 'Accept: application/json' "$TEMPO/api/traces/$id" | python3 -c '
import json, sys
d = json.load(sys.stdin)
names = set()
for rs in d.get("batches") or d.get("resourceSpans") or []:
    for a in rs.get("resource", {}).get("attributes", []):
        if a["key"] == "service.name":
            names.add(a["value"].get("stringValue", ""))
print(sys.argv[1], *sorted(names))
' "$id"
  done
}

# Spans arrive in batches (SDK, then Collector), so the trace may take a few seconds to complete.
result=""
for _ in $(seq 1 15); do
  result=$(traces_of_order)
  while read -r id services; do
    [ -n "$id" ] || continue
    missing=""
    for svc in $REQUIRED; do
      grep -qw -- "$svc" <<<"$services" || missing="$missing $svc"
    done
    if [ -z "$missing" ]; then
      echo "trace $id"
      echo "services: $services"
      echo "TRACE OK"
      exit 0
    fi
  done <<<"$result"
  sleep 2
done

echo "traces found for order $order_id:" >&2
echo "${result:-  none}" >&2
echo "TRACE FAILED: no trace of order $order_id contains all of: $REQUIRED" >&2
exit 1
