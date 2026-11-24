#!/usr/bin/env bash
# Finds the trace of one order in Tempo (TraceQL on span attribute order.id) and checks that it
# spans orders, inventory, payments and notifications. Then checks that Loki holds the orders log
# line "order created" carrying that trace id. Prints the trace id, its services and the log line.
# Usage: scripts/trace-check.sh <order_id>
# TRACE_CHECK_TIMEOUT (seconds, default 60) bounds each of the two waits.
set -euo pipefail

order_id=${1:?usage: $0 <order_id>}
TIMEOUT=${TRACE_CHECK_TIMEOUT:-60}
TEMPO=http://localhost:13200
LOKI=http://localhost:13100
REQUIRED="orders inventory payments notifications"
K=(kubectl --context kind-order-saga -n observability)

pids=()
cleanup() {
  local p
  for p in "${pids[@]+"${pids[@]}"}"; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# port_forward <service> <local port> <remote port>: forwards and waits up to 10 s for /ready;
# aborts the script if the backend never answers.
port_forward() {
  local svc=$1 lport=$2 rport=$3
  "${K[@]}" port-forward "svc/$svc" "$lport:$rport" >/dev/null 2>&1 &
  pids+=($!)
  for _ in $(seq 1 50); do
    curl -fsS -o /dev/null "http://localhost:$lport/ready" 2>/dev/null && return 0
    sleep 0.2
  done
  echo "TRACE FAILED: $svc is not ready on localhost:$lport after 10 s (is svc/$svc running in observability?)" >&2
  exit 1
}

# Prints "<trace id> <service> <service>..." for every trace that matches the order id. A failed or
# partial fetch prints nothing for that trace (the caller retries), never a traceback.
traces_of_order() {
  local now ids id
  now=$(date +%s)
  ids=$(curl -fsS -G "$TEMPO/api/search" \
    --data-urlencode "q={ span.order.id = \"$order_id\" }" \
    --data-urlencode "start=$((now - 3600))" --data-urlencode "end=$((now + 60))" \
    --data-urlencode "limit=20" 2>/dev/null |
    python3 -c '
import json, sys
try:
    for t in json.load(sys.stdin).get("traces") or []:
        print(t["traceID"])
except Exception:
    pass
') || true
  for id in $ids; do
    curl -fsS -H 'Accept: application/json' "$TEMPO/api/traces/$id" 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
names = set()
for rs in d.get("batches") or d.get("resourceSpans") or []:
    for a in rs.get("resource", {}).get("attributes", []):
        if a.get("key") == "service.name":
            names.add(a.get("value", {}).get("stringValue", ""))
# Tempo drops leading zeros from trace ids; Loki stores the full 32 hex digits.
print(sys.argv[1].rjust(32, "0"), *sorted(names))
' "$id" || true
  done
}

# Prints the orders log lines in Loki that carry the given trace id, one per line.
logs_of_trace() {
  local trace_id=$1 now
  now=$(date +%s)
  curl -fsS -G "$LOKI/loki/api/v1/query_range" \
    --data-urlencode "query={service_name=\"orders\"} | trace_id=\"$trace_id\"" \
    --data-urlencode "start=$((now - 3600))000000000" --data-urlencode "end=$((now + 60))000000000" \
    --data-urlencode "limit=100" 2>/dev/null | python3 -c '
import json, sys
try:
    for s in json.load(sys.stdin)["data"]["result"]:
        for _, line in s.get("values", []):
            print(line)
except Exception:
    pass
' || true
}

port_forward tempo 13200 3200
port_forward loki 13100 3100

# Spans arrive in batches (SDK, then Collector), so the trace may take a while to complete.
result="" trace_id="" services=""
deadline=$((SECONDS + TIMEOUT))
while [ -z "$trace_id" ] && [ "$SECONDS" -lt "$deadline" ]; do
  result=$(traces_of_order)
  while read -r id svcs; do
    [ -n "$id" ] || continue
    missing=""
    for svc in $REQUIRED; do
      grep -qw -- "$svc" <<<"$svcs" || missing="$missing $svc"
    done
    if [ -z "$missing" ]; then
      trace_id=$id services=$svcs
      break
    fi
  done <<<"$result"
  [ -n "$trace_id" ] || sleep 2
done

if [ -z "$trace_id" ]; then
  echo "traces found for order $order_id:" >&2
  echo "${result:-  none}" >&2
  echo "TRACE FAILED: no trace of order $order_id contains all of: $REQUIRED (waited ${TIMEOUT}s)" >&2
  exit 1
fi
echo "trace $trace_id"
echo "services: $services"
echo "TRACE OK"

# Logs are batched the same way, so the line may also take a few seconds to be queryable.
logs="" line=""
deadline=$((SECONDS + TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  logs=$(logs_of_trace "$trace_id")
  line=$(grep -m1 -F 'order created' <<<"$logs" || true)
  [ -n "$line" ] && break
  sleep 2
done

if [ -z "$line" ]; then
  echo "orders log lines with trace_id $trace_id:" >&2
  echo "${logs:-  none}" >&2
  echo "LOGS FAILED: no orders log line 'order created' with trace_id $trace_id in Loki (waited ${TIMEOUT}s)" >&2
  exit 1
fi
echo "log: $line"
echo "LOGS OK"
