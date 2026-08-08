# READY-FOR-REVIEW — FAC-222

**Ticket:** FAC-222
**Branch:** herd/fac-222
**Diff:** 8 files changed, 482 insertions(+), 10 deletions(-)

## Fixes applied

### BLOCKING #1 — Inverted feedbackInterval formula

**Root cause:** `cmd/herd/main.go:5560` computed `*interval * 60 / int(feedback.DefaultInterval.Seconds())` = `15 * 60 / 1800 = 0`, clamped to 1. The census ran every tick instead of every 120 ticks (30 min).

**Fix:** Extracted `feedback.CensusTickInterval(tickIntervalSeconds int)` into `pkg/feedback/census.go` with the correct formula: `int(DefaultInterval.Seconds()) / tickIntervalSeconds` = `1800 / 15 = 120`. Replaced the inline formula in `cmd/herd/main.go` with a call to this function.

**Non-vacuous test:** `TestCensusTickInterval` in `pkg/feedback/census_test.go` — table-driven with 7 cases (15s→120, 30s→60, 60s→30, 1800s→1, 3600s→1, 0→1, -5→1). Proven to FAIL on the old formula: `CensusTickInterval(15) = 1, want 120`.

### BLOCKING #2 — coordinator.Register hardcodes the name

**Root cause:** `Register(root, workspace string)` hardcoded `name := CoordinatorName` and accepted no name parameter, making `ReplyTarget.Name`, `Dispatcher.CoordinatorName`, and the non-default branch of `coordinatorName()` structurally dead in production.

**Fix:** Changed signature to `Register(root, name, workspace string)`. When `name` is empty, `CoordinatorName` is used as default — matching the doc comment's original intent. Updated the call site in `cmd/herd/main.go` to pass `coordinator.CoordinatorName` explicitly. Updated all 5 existing tests to pass `""` for the name parameter.

**Non-vacuous test:** `TestRegisterCustomNameFlowsThroughResolve` in `pkg/coordinator/registration_test.go` — registers with `"forge-coordinator"`, resolves, and asserts the custom name survives the round-trip. Proven to FAIL on the old hardcode: `Register name = "coordinator", want "forge-coordinator"`.

### INFORMATIONAL #3 and #4 — OUT OF SCOPE

- #3 (Resolve error silently swallowed by dispatch): noted, left unchanged.
- #4 (no coordinator.json cleanup on loop exit): noted, left unchanged.

## Verification

- `go build ./...` — exit 0
- `go test ./pkg/feedback/ ./pkg/coordinator/ ./pkg/daemon/ ./pkg/dispatch/ -v -count=1` — all FAC-222 tests PASS; `TestDispatch_PackageCwdNotPolluted` is the pre-existing flaky test the reviewer already confirmed fails on the base commit
- `make lint` (go vet + hermeticity scanner) — exit 0

## Non-vacuity proof

Both tests were run against temporarily-broken code and confirmed to FAIL before the fix was restored:
- `TestCensusTickInterval`: FAIL `CensusTickInterval(15) = 1, want 120`
- `TestRegisterCustomNameFlowsThroughResolve`: FAIL `Register name = "coordinator", want "forge-coordinator"`
