# obol — build & quality targets. See CLAUDE.md.
BINARY      := obold
PKG         := ./...
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
# Prefer a golangci-lint binary on PATH (matches CI's pinned v2.12.2); otherwise
# fetch that version on demand. The bare module path has no go.mod entry, so the
# version pin is required for `go run` to resolve it.
GOLANGCI    := $(shell command -v golangci-lint 2>/dev/null || echo "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2")

.PHONY: build test race lint cover fmt fmt-check vet tidy check clean

build: ## build obold into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test: ## run all tests
	go test $(PKG)

race: ## run all tests under the race detector (REQUIRED for concurrency changes)
	go test -race -count=1 $(PKG)

lint: ## run golangci-lint (v2)
	$(GOLANGCI) run

fmt: ## format code
	gofmt -s -w .

fmt-check: ## fail if code is not gofmt-clean
	@diff=$$(gofmt -s -l .); if [ -n "$$diff" ]; then echo "gofmt needed:"; echo "$$diff"; exit 1; fi

vet: ## go vet
	go vet $(PKG)

cover: ## coverage report for the kernel
	go test -covermode=atomic -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -1

tidy: ## tidy and verify modules
	go mod tidy
	go mod verify

check: fmt-check vet lint race ## everything CI runs; run before pushing

clean:
	rm -rf bin dist coverage.* *.out
