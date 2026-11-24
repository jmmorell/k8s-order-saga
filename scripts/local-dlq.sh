#!/usr/bin/env bash
# Prints the message count of every JetStream stream in the kind cluster, read
# from the NATS monitor (pod nats-0, port 8222; the nats Service only exposes 4222).
set -euo pipefail

pf=""
cleanup() { if [ -n "$pf" ]; then kill "$pf" 2>/dev/null || true; fi; }
trap cleanup EXIT

kubectl --context kind-order-saga -n platform port-forward pod/nats-0 18222:8222 >/dev/null 2>&1 &
pf=$!

for _ in $(seq 1 50); do
  curl -fsS -o /dev/null http://localhost:18222/healthz 2>/dev/null && break
  sleep 0.2
done

curl -fsS 'http://localhost:18222/jsz?streams=true' | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(s["name"], s["state"]["messages"]) for a in d.get("account_details", []) for s in a.get("stream_detail", [])]'
