.PHONY: test test-short lint build

test:
	go test -race ./...

test-short:
	go test -short -race ./...

lint:
	golangci-lint run

build:
	go build ./...
