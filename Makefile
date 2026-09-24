# Builds run in a container so the result is the same on any machine with
# Docker — no Go toolchain to install, no version to disagree about.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= docker run --rm -v $(CURDIR):/src -w /src -e CGO_ENABLED=0 golang:1.25-alpine go
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.DefaultHost=$(DEFAULT_HOST)

.PHONY: all test clean
all: dist/norriva-darwin-arm64 dist/norriva-darwin-amd64 dist/norriva-linux-amd64 dist/norriva-linux-arm64

dist/norriva-%: $(shell find . -name '*.go') go.mod
	@mkdir -p dist
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) \
	  $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $@ .
	@echo built $@

test:
	$(GO) vet ./... && $(GO) test ./...

clean:
	rm -rf dist
