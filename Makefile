# obol — build & quality targets. See CLAUDE.md.
BINARY      := obold
PKG         := ./...
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
# Prefer a golangci-lint binary on PATH (matches CI's pinned v2.12.2); otherwise
# fetch that version on demand. The bare module path has no go.mod entry, so the
# version pin is required for `go run` to resolve it.
GOLANGCI    := $(shell command -v golangci-lint 2>/dev/null || echo "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2")

.PHONY: build test race lint cover fmt fmt-check vet tidy check clean integ-docker integ-pcluster

build: ## build obold + obol into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/obold ./cmd/obold
	go build -ldflags "$(LDFLAGS)" -o bin/obol ./cmd/obol

test: ## run all tests
	go test $(PKG)

integ-docker: ## build the single-node Slurm image and run the containerized seam tests
	@command -v docker >/dev/null 2>&1 || { echo "integ-docker: docker not found; skipping"; exit 0; }
	@mkdir -p test/docker/bin
	GOOS=linux GOARCH=$(shell go env GOARCH) go build -o test/docker/bin/obold ./cmd/obold
	GOOS=linux GOARCH=$(shell go env GOARCH) go build -o test/docker/bin/obol  ./cmd/obol
	go test -tags=docker_integration -count=1 -v -timeout 15m ./test/docker/

integ-pcluster: ## run the AWS ParallelCluster integration test (needs OBOL_INTEG_* env; skips otherwise)
	go test -tags=integration -count=1 -v -timeout 20m ./test/integration/

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
