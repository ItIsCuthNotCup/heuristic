.PHONY: build test check install

build:
	go build -trimpath -o bin/heuristic ./cmd/heuristic
	go build -trimpath -o bin/heu ./cmd/heu

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }

install:
	go install ./cmd/heu ./cmd/heuristic
