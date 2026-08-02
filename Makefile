# Herdforge Makefile

.PHONY: all build test test-unit test-coverage test-mutation preflight lint self-test herd-up clean

all: preflight test build

build:
	@echo "==> Building herd binary..."
	@mkdir -p bin
	go build -o bin/herd ./cmd/herd

test: test-unit

test-unit:
	@echo "==> Running full unit test suite..."
	go test -count=1 -timeout=180s ./...

test-coverage:
	@echo "==> Running coverage analysis..."
	go test -coverprofile=/tmp/herd-cover.out -count=1 -timeout=180s ./...
	go tool cover -func=/tmp/herd-cover.out

test-mutation:
	@echo "==> Running mutation checks on high-risk packages..."
	@for pkg in verifier review config; do \
		echo "--- $$pkg ---"; \
		go test -count=1 -timeout=120s ./pkg/$$pkg/; \
	done

preflight:
	@echo "==> Running preflight workspace boundary checks..."
	go run ./cmd/herd preflight

self-test: build
	@echo "==> Running compiled Herdforge self-test suite against ITSELF..."
	./bin/herd selftest

herd-up: build
	@echo "==> Spawning Herdforge autonomous software factory daemon..."
	./bin/herd pulse --act --spawn

lint:
	@echo "==> Running go vet static analysis..."
	go vet ./...

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin
