sha: d96dc305113562e800b331b6728c466f06a09ebc
branch: recovery/fac-744-generation-recovery
task: FAC-744
reviewer: review-fac-744-d96dc3051135
reviewer-family: anthropic
builder-family: google
reviewed-base: 1656bb778e9afbcf03419a844e62253d699fe5df
reviewed-head: d96dc305113562e800b331b6728c466f06a09ebc
verdict: PASS
---

## Review Summary

FAC-744 is a bounded mechanical test correction for `pkg/verifier/mutation_command_timeout_test.go` that stabilizes the `TestRunMutationCheck_CommandDeadlineExcludesPreparation` test against flakes on slow/virtualized environments like WSL. This verdict includes production mutation proof of non-vacuity.

## Problem Statement

The test was failing on CI due to an aggressive 100ms command timeout on virtualized hardware where subprocess execution takes ~300ms. The timeout was firing during command execution rather than being reserved for the command phase only, causing false `OutcomeBLOCKED` results.

## Root Cause

The test was conflating two distinct time budgets:
1. **Preparation phase**: mutex-locked initialization before command execution
2. **Command execution**: the actual subprocess execution window

With a 100ms timeout, slow systems would exhaust the budget during (1), then fail (2).

## Solution Analysis

The fix decouples these by:

1. **Increasing command timeout** (100ms → 2s): Provides ample headroom for actual command execution on any hardware
2. **Adding explicit deadline assertion** in `beforeCommandStart`: Converts silent early-return behavior to an error if deadline is detected during preparation phase, proving the invariant
3. **Adding 200ms sleep after preparation gate**: Simulates realistic preparation work and stresses the deadline boundary  
4. **Adding DiskAdmission mocks** to `TestRunMutationCheck_ParentCancelWhileQueuedStartsNoChild` and `TestRunMutationCheck_SlotWaitExhaustionStartsNoCommand`: Prevents disk pressure starvation of slot-holding test routines

## Non-Vacuity Proof: Production Mutation Test

### Mutation Applied
Applied production code mutation to `pkg/verifier/verifier.go` line 261:

**Before (original, correct):**
```go
if v.beforeCommandStart != nil {
    v.beforeCommandStart(commandCtx)
}
```

**After (mutated, violates invariant):**
```go
var prepCtx context.Context = commandCtx
if commandTimeout > 0 {
    var cancel context.CancelFunc
    prepCtx, cancel = context.WithTimeout(commandCtx, commandTimeout)
    defer cancel()
}
if v.beforeCommandStart != nil {
    v.beforeCommandStart(prepCtx)
}
```

This mutation wraps `commandCtx` with a deadline **before preparation**, directly violating the invariant that preparation must exclude the command deadline.

### Test Execution: Mutation RED (Fails)

```
env -u GOROOT -u HERD_ROOT -u HERD_REPO_ROOT GOMAXPROCS=2 GOFLAGS=-p=2 go test ./pkg/verifier -run '^TestRunMutationCheck_CommandDeadlineExcludesPreparation$' -count=1 -v
```

**Exit**: `1` (FAIL)

**Output**:
```
=== RUN   TestRunMutationCheck_CommandDeadlineExcludesPreparation
    mutation_command_timeout_test.go:166: commandCtx has deadline during preparation: preparation must exclude command deadline
--- FAIL: TestRunMutationCheck_CommandDeadlineExcludesPreparation (0.33s)
FAIL
FAIL	github.com/Kampe/Herdforge/pkg/verifier	0.558s
```

### Test Execution: Original Code GREEN (Passes)

```
env -u GOROOT -u HERD_ROOT -u HERD_REPO_ROOT GOMAXPROCS=2 GOFLAGS=-p=2 go test ./pkg/verifier -run '^TestRunMutationCheck_CommandDeadlineExcludesPreparation$' -count=1 -v
```

**Exit**: `0` (PASS)

**Output**:
```
=== RUN   TestRunMutationCheck_CommandDeadlineExcludesPreparation
--- PASS: TestRunMutationCheck_CommandDeadlineExcludesPreparation (0.33s)
PASS
ok	github.com/Kampe/Herdforge/pkg/verifier	0.552s
```

## Verification Summary

### Tests Run (Focused Suite)

```
env -u GOROOT -u HERD_ROOT -u HERD_REPO_ROOT GOMAXPROCS=2 GOFLAGS=-p=2 go test ./pkg/verifier -run '^TestRunMutationCheck_(QueuedVacuousMutantFailsAfterSlotWait|CommandDeadlineExcludesPreparation|ParentCancelWhileQueuedStartsNoChild|SlotWaitExhaustionStartsNoCommand)$' -count=1 -v
```

**Exit**: `0` (GREEN - all 4 tests PASS)

### Build

```
env -u GOROOT -u HERD_ROOT -u HERD_REPO_ROOT GOMAXPROCS=2 GOFLAGS=-p=2 go build ./...
```

**Exit**: `0` (SUCCESS)

### Lint & Static Analysis

All checks passed:
- ✓ binparity: 53 executable dispositions
- ✓ workspace boundary check: zero absolute path leaks
- ✓ signal-literal check: no host-wide kill literals
- ✓ merge-policy check: required CI and different-family review declared

## Acceptance Criteria Met

- ✅ **Deadline assertion tests invariant**: Line 165-167 in test explicitly checks `ctx.Deadline()` and fails if deadline exists
- ✅ **Production mutation RED**: When deadline is applied before preparation (line 260-265 in production code), the test fails with the exact assertion error
- ✅ **Original code GREEN**: Without the mutation, test passes and deadline is NOT visible during preparation
- ✅ **No fallback behavior**: The assertion errors out; there's no silent return or toleration of the violation
- ✅ **Focused tests pass**: All 4 tests green, no production code changes to core verifier logic
- ✅ **Deterministic proof**: Mutation directly violates the invariant; test catches it 100% of the time

## Conclusion

This is a **PASS** verdict with proof of non-vacuity. The change is:
- **Mechanically sound**: Correct decoupling of preparation and command timeout budgets
- **Non-vacuous verified**: Production mutation proof shows test RED with deadline-during-prep, GREEN without it
- **Production-grade**: Uses real `context.WithTimeout` mutation, not arbitrary test widening
- **Properly asserted**: Explicit deadline check prevents silent failures
- **Risk level**: Very low; test-only change addressing known flake with proven detection capability

---

## Metadata

patch_id: 39688cbd7f3a6cff7b0a84e2f1d26f298bc2b102
verification_digest: sha256:b00a0056a74a44c1ba986c08d893bbc4818d6bc7b121f18e279224bc10779326
full_range: 1656bb778e9afbcf03419a844e62253d699fe5df..d96dc305113562e800b331b6728c466f06a09ebc
mutation_proof: context.WithTimeout applied before beforeCommandStart (line 260-265 pkg/verifier/verifier.go)
