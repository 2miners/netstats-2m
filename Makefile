# Go netstats client: `make build`, `make test`, `make release`.
BIN     := build/bin/netstats-2m
VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)

export GOTOOLCHAIN := local

.PHONY: build test

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o $(BIN) .

test:
	go vet ./... && go test ./...

