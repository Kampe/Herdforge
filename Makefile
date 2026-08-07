# Herdforge Makefile

.PHONY: all build test test-unit test-contracts test-hermetic-compile test-coverage test-mutation preflight lint self-test herd-up clean ci

all: preflight test build

# ci mirrors .github/workflows/ci.yml EXACTLY, with a hermetic git
# environment (no user/system gitconfig) so a dev machine's config cannot
# mask environment-dependent tests. Run this before EVERY push — if ci is
# red here it is red on the runner.
ci:
	@echo "==> Running CI-equivalent gate (hermetic git env)..."
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null $(MAKE) lint test-unit preflight
	@echo "==> CI gate PASSED"

build:
	@echo "==> Building herd binary..."
	@mkdir -p bin
	go build -o bin/herd ./cmd/herd

test: test-unit

# Compile (do not run) the FAC-151 hermetic integration profile. This is the
# exact go-test -c step hermeticDockerRunner.compile uses inside the container.
# Without it, ordinary `go test ./pkg/verifier` can stay green while the
# quarantined ownership suite fails to build under the hermetic tag (FAC-198).
test-hermetic-compile:
	@echo "==> Compiling FAC-151 hermetic profile (tag fac151_hermetic_integration)..."
	@tmpdir=$$(mktemp -d) && \
		go test -c -tags fac151_hermetic_integration -o "$$tmpdir/verifier.test" ./pkg/verifier && \
		rm -rf "$$tmpdir" && \
		echo "==> Hermetic profile compiles"

test-unit: test-contracts test-hermetic-compile
	@echo "==> Running full unit test suite..."
	go test -count=1 -timeout=180s ./...

# contracts/agentscope is a nested, independently consumable Go module. The
# root module's `go test ./...` skips nested modules, so this target makes the
# contract gate explicit and prevents the nested module from being silently
# omitted by `make ci` and CI.
test-contracts:
	@echo "==> Running nested contract module tests (contracts/agentscope)..."
	cd contracts/agentscope && go test -count=1 -timeout=180s ./...

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
	@echo "==> Running go vet on nested contract module (contracts/agentscope)..."
	cd contracts/agentscope && go vet ./...

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin
