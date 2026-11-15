# Go netstats client: `make build`, `make test`, `make release`.
BIN     := build/bin/netstats-2m
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

export GOTOOLCHAIN := local

.PHONY: build test release

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o $(BIN) .

test:
	go vet ./... && go test ./...

# release archives for linux amd64/arm64 in build/release, then:
#   gh release create $(VERSION) build/release/*
RELEASE := build/release
release:
	mkdir -p $(RELEASE)
	for arch in amd64 arm64; do \
		name=netstats-2m-$(VERSION)-linux-$$arch; \
		mkdir -p $(RELEASE)/stage/$$name && \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o $(RELEASE)/stage/$$name/netstats-2m . && \
		cp README.md LICENSE example.env deploy/netstats-2m@.service $(RELEASE)/stage/$$name/ && \
		tar -C $(RELEASE)/stage -czf $(RELEASE)/$$name.tar.gz $$name || exit 1; \
	done
	cd $(RELEASE) && shasum -a 256 *.tar.gz > SHA256SUMS
