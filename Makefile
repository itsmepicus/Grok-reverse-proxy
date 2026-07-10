.PHONY: build test check run

build:
	go build -trimpath -o bin/grok-reverse-proxy ./cmd/grok-reverse-proxy

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

run:
	go run ./cmd/grok-reverse-proxy
