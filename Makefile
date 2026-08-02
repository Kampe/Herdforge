# Herdforge Makefile

.PHONY: all build test preflight lint clean

all: preflight test build

build:
	@echo "==> Building herd binary..."
	@mkdir -p bin
	go build -o bin/herd ./cmd/herd

test:
	@echo "==> Running full unit test suite..."
	go test -v ./...

preflight:
	@echo "==> Running preflight workspace boundary checks..."
	go run ./cmd/herd preflight

lint:
	@echo "==> Running go vet static analysis..."
	go vet ./...

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin .herd
