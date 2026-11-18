.PHONY: test test-short lint build up down logs smoke

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
