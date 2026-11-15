.PHONY: test test-short lint build up down logs smoke order dlq chart-test local-up local-down local-status local-smoke local-dlq local-reload

test:
	go test -race ./...

test-short:
	go test -short -race ./...

lint:
	golangci-lint run

build:
	go build ./...

# Creates .env with a random database password on first use.
up:
	@test -f .env || { sed "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$$(openssl rand -hex 16)/" .env.example > .env; echo "created .env"; }
	docker compose up --build -d

down:
	docker compose down --volumes

logs:
	docker compose logs --follow --tail=100

smoke:
	./scripts/smoke.sh

# Places one order and prints it. ORDERS_URL overrides the target.
order:
	@url=$${ORDERS_URL:-http://localhost:8080}; \
	id=$$(curl -fsS -X POST "$$url/orders" -H 'Content-Type: application/json' \
	  -d '{"customer_email":"make@example.local","items":[{"sku":"SKU-001","qty":1}]}' | sed -nE 's/.*"id":"([^"]+)".*/\1/p'); \
	test -n "$$id" || { echo "POST $$url/orders returned no order id" >&2; exit 1; }; \
	sleep 1; curl -fsS "$$url/orders/$$id"; echo

# Prints the message count of every JetStream stream (Compose NATS monitor).
dlq:
	@curl -fsS 'http://localhost:8222/jsz?streams=true' | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(s["name"], s["state"]["messages"]) for a in d.get("account_details", []) for s in a.get("stream_detail", [])]'

# Lints, renders and validates charts/service for every service; needs helm and kubeconform, no cluster.
chart-test:
	./scripts/chart-test.sh

# Creates the kind cluster order-saga with NATS, Postgres, Mailpit, Envoy Gateway and the five services. Idempotent.
local-up:
	./scripts/local-up.sh

local-down:
	./scripts/local-down.sh

local-status:
	kubectl --context kind-order-saga get nodes,pods -A -o wide

# Runs the smoke test against the kind cluster (orders via the gateway, Mailpit via port-forward).
local-smoke:
	./scripts/local-smoke.sh

# Prints the message count of every JetStream stream in the kind cluster.
local-dlq:
	./scripts/local-dlq.sh

# Rebuilds the five images, loads them into kind and restarts the Deployments.
local-reload:
	./scripts/local-up.sh reload
