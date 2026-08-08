# Herdforge Makefile

.PHONY: all build test test-unit test-contracts test-hermetic-compile test-coverage test-mutation test-race test-e2e preflight lint self-test herd-up clean ci

# FAC-135: shared hermetic Git environment for every gate. Host signing, hooks,
# and ambient credentials must not influence fixtures or coverage.
HERMETIC_GIT := GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
COVER_DIR ?= $(CURDIR)/.herd/coverage
COVER_PROFILE ?= $(COVER_DIR)/cover.out

all: preflight test build

# ci mirrors .github/workflows/ci.yml EXACTLY, with a hermetic git
# environment (no user/system gitconfig) so a dev machine's config cannot
# mask environment-dependent tests. Run this before EVERY push — if ci is
# red here it is red on the runner.
ci:
	@echo "==> Running CI-equivalent gate (hermetic git env)..."
	$(HERMETIC_GIT) $(MAKE) lint test-unit test-race preflight
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

# Execute the quarantined FAC-151 ownership suite inside the pinned hermetic
# Docker container (linux/$(GOARCH)). Requires Docker and a clean tracked tree.
# Wired into CI as a separate job so the unit gate stays Docker-free.
test-hermetic-fac151: build test-hermetic-compile
	@echo "==> Running FAC-151 hermetic Docker profile (herd verify-fac151)..."
	./bin/herd verify-fac151

test-unit: test-contracts test-hermetic-compile
	@echo "==> Running full unit test suite..."
	# 300s: cmd/herd integration builds the binary multiple times; 180s is
	# flaky/red on both main and this branch (pre-existing, not FAC-198).
	$(HERMETIC_GIT) go test -count=1 -timeout=300s ./...

# contracts/agentscope is a nested, independently consumable Go module. The
# root module's `go test ./...` skips nested modules, so this target makes the
# contract gate explicit and prevents the nested module from being silently
# omitted by `make ci` and CI.
test-contracts:
	@echo "==> Running nested contract module tests (contracts/agentscope)..."
	$(HERMETIC_GIT) sh -c 'cd contracts/agentscope && go test -count=1 -timeout=180s ./...'

# FAC-135: coverage under a repo-local profile (not host /tmp) plus non-vacuous
# package floors for core production paths.
test-coverage:
	@echo "==> Running coverage analysis (hermetic git env)..."
	@mkdir -p "$(COVER_DIR)"
	$(HERMETIC_GIT) go test -coverprofile="$(COVER_PROFILE)" -count=1 -timeout=300s ./...
	go tool cover -func="$(COVER_PROFILE)"
	@echo "==> Enforcing coverage floors..."
	go run ./scripts/check-coverage-floors.go "$(COVER_PROFILE)"

# FAC-135: race detector on core orchestration packages (non-vacuous gate).
test-race:
	@echo "==> Running race detector on core packages..."
	$(HERMETIC_GIT) go test -race -count=1 -timeout=300s ./pkg/lifecycle/ ./pkg/daemon/ ./pkg/claim/ ./pkg/mail/ ./pkg/lock/

test-mutation:
	@echo "==> Running mutation checks on high-risk packages (hermetic)..."
	@for pkg in verifier review config preflight; do \
		echo "--- $$pkg ---"; \
		$(HERMETIC_GIT) go test -count=1 -timeout=120s ./pkg/$$pkg/; \
	done

# FAC-135: compiled-driver factory conformance (cliForgeDriver + fakes).
test-e2e:
	@echo "==> Running factory e2e (compiled herd + protocol fakes)..."
	$(HERMETIC_GIT) go test -count=1 -timeout=300s -run 'FactoryE2E_|CliForgeDriver_' ./cmd/herd/

preflight:
	@echo "==> Running preflight workspace boundary checks..."
	$(HERMETIC_GIT) go run ./cmd/herd preflight

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
	rm -rf bin "$(COVER_DIR)"
