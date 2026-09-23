.PHONY: build test check

build:
	go build -trimpath -o bin/metacog-agent ./cmd/metacog-agent

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }
