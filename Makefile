# Herdforge Makefile

.PHONY: all build test test-unit test-contracts test-hermetic-compile test-coverage test-mutation test-race test-e2e preflight lint security security-test security-deps self-test herd-up clean ci package-inventory bin-parity known-failures check-go-toolchain

# FAC-135: shared hermetic Git environment for every gate. Host signing, hooks,
# and ambient credentials must not influence fixtures or coverage.
HERMETIC_GIT := GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
COVER_DIR ?= $(CURDIR)/.herd/coverage
COVER_PROFILE ?= $(COVER_DIR)/cover.out

# build runs BEFORE test: pkg/security resolves the herd binary as bin/herd
# relative to cwd (readiness_attest.go), so tests that shell out to it must run
# against current source. With test first, they probed whatever binary happened
# to be left over from a previous build — a stale bin/herd predating a new
# subcommand failed TestParentDeath_RealLauncherExit with "unknown subcommand
# 'netbroker-serve'" while CI, which always builds first, stayed green.
all: preflight build test

# ci mirrors .github/workflows/ci.yml EXACTLY, with a hermetic git
# environment (no user/system gitconfig) so a dev machine's config cannot
# mask environment-dependent tests. Run this before EVERY push — if ci is
# red here it is red on the runner.
ci:
	@echo "==> Running CI-equivalent gate (hermetic git env)..."
	$(HERMETIC_GIT) $(MAKE) lint test-unit test-race preflight
	@echo "==> CI gate PASSED"

# FAC-486: must precede every go invocation. pkg/preflight.CheckGoToolchain
# diagnoses the same condition, but reaching it requires compiling Go — which is
# exactly what a GOROOT mismatch breaks — so it can only ever fire from an
# already-built binary. This target is that check at the pre-compile layer.
check-go-toolchain:
	@./scripts/check-go-toolchain.zsh

build: check-go-toolchain
	@echo "==> Building herd binary..."
	@./scripts/build-herd.zsh "$$(pwd -P)"

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

test-unit: check-go-toolchain test-contracts test-hermetic-compile
	@echo "==> Running full unit test suite..."
	# 300s: cmd/herd integration builds the binary multiple times; 180s is
	# flaky/red on both main and this branch (pre-existing, not FAC-198).
	$(HERMETIC_GIT) go test -count=1 -shuffle=on -timeout=300s ./...

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

# FAC-490: compare the checked-in known-failure set against the actual baseline
# run. The test command may exit non-zero by design; the exact-set comparison is
# the gate that follows it.
known-failures:
	@report=$$(mktemp); trap 'rm -f "$$report"' EXIT; \
		$(HERMETIC_GIT) go test -json -count=1 -shuffle=on -timeout=300s ./cmd/herd/ ./pkg/verifier/ >"$$report" 2>&1 || true; \
		$(HERMETIC_GIT) go run ./scripts/knownfailures --manifest .herd/known-failures.json --report "$$report"

preflight: check-go-toolchain
	@echo "==> Running preflight workspace boundary, merge-policy, and main/origin drift checks..."
	$(HERMETIC_GIT) go run ./cmd/herd preflight

self-test: build
	@echo "==> Running compiled Herdforge self-test suite against ITSELF..."
	./bin/herd selftest

herd-up: build
	@echo "==> Spawning Herdforge autonomous software factory daemon..."
	./bin/herd pulse --act --spawn

lint: check-go-toolchain
	@echo "==> Running go vet static analysis..."
	go vet ./...
	@echo "==> Running go vet on nested contract module (contracts/agentscope)..."
	cd contracts/agentscope && go vet ./...
	@echo "==> Running test hermeticity scan (FAC-215)..."
	go run ./scripts/hermeticity/
	@echo "==> Checking package inventory drift (FAC-301)..."
	$(MAKE) package-inventory
	$(MAKE) security
	$(MAKE) security-deps
	$(MAKE) bin-parity

bin-parity:
	@echo "==> Checking Chainseer executable parity provenance (FAC-309)..."
	@parity_dir="$${CHAINSEER_BIN:-../../../../chainseer/bin}"; \
	if [ -d "$$parity_dir" ]; then \
		CHAINSEER_BIN="$$parity_dir" $(HERMETIC_GIT) go run ./scripts/binparity; \
	elif [ "$${CHAINSEER_PARITY_OPTIONAL:-0}" = "1" ]; then \
		echo "bin-parity: SKIP — optional Chainseer source unavailable at $$parity_dir (CI-only opt-in)"; \
	else \
		echo "bin-parity: FAIL — Chainseer source unavailable at $$parity_dir (set CHAINSEER_BIN for an authorized source)" >&2; \
		exit 1; \
	fi

security:
	./scripts/security-gate.zsh

security-test:
	./scripts/security-gate_test.zsh

# Scan the deterministically sorted set of tracked Go modules. The script uses
# `git ls-files`, so untracked .worktrees and .herd runtime trees are never
# traversed; it has no severity or vulnerability suppressions.
security-deps:
	./scripts/check-go-dependencies.zsh

# FAC-301: graph-backed package reachability/classification inventory. Validates
# the live import graph against the checked-in baseline, catching both broken
# wiring (production package regresses to unwired) and unintended growth (new
# unwired package not recorded in baseline). The baseline lives at
# scripts/packageinventory/baseline.json; regenerate with:
#   go run ./scripts/packageinventory/ --generate scripts/packageinventory/baseline.json
package-inventory:
	@echo "==> Checking package reachability inventory (FAC-301)..."
	$(HERMETIC_GIT) go run ./scripts/packageinventory/ --check scripts/packageinventory/baseline.json

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin "$(COVER_DIR)"
