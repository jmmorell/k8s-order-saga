#!/usr/bin/env bash
# Posts one order that must be confirmed and one that must be cancelled, then
# checks that both outcomes reached Mailpit. Needs the Compose stack running.
set -euo pipefail

ORDERS=${ORDERS_URL:-http://localhost:8080}
MAILPIT=${MAILPIT_URL:-http://localhost:8025}

fail() { echo "SMOKE FAILED: $*" >&2; exit 1; }

wait_for() { # description, command...
  local what=$1; shift
  for _ in $(seq 1 100); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  fail "timed out waiting for $what"
}

place_order() { # sku -> order id
  curl -fsS -X POST "$ORDERS/orders" -H 'Content-Type: application/json' \
    -d "{\"customer_email\":\"smoke@example.local\",\"items\":[{\"sku\":\"$1\",\"qty\":1}]}" |
    sed -E 's/.*"id":"([^"]+)".*/\1/'
}

order_has_status() { # id status
  curl -fsS "$ORDERS/orders/$1" | grep -q "\"status\":\"$2\""
}

mail_count() {
  curl -fsS "$MAILPIT/api/v1/messages" | sed -E 's/.*"total":([0-9]+).*/\1/'
}

at_least_two_mails() { [ "$(mail_count)" -ge 2 ]; }

wait_for "orders to be ready" curl -fsS "$ORDERS/readyz"

confirmed=$(place_order SKU-001)
[ -n "$confirmed" ] || fail "no id returned for the SKU-001 order"
wait_for "order $confirmed to be CONFIRMED" order_has_status "$confirmed" CONFIRMED
echo "order $confirmed CONFIRMED"

cancelled=$(place_order SKU-010)
[ -n "$cancelled" ] || fail "no id returned for the SKU-010 order"
wait_for "order $cancelled to be CANCELLED" order_has_status "$cancelled" CANCELLED
echo "order $cancelled CANCELLED"

wait_for "at least 2 emails in Mailpit" at_least_two_mails
echo "Mailpit has $(mail_count) messages"
echo "SMOKE OK"
