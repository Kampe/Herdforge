# READY-FOR-REVIEW — FAC-222

**Ticket:** FAC-222
**Branch:** herd/fac-222
**Diff:** 10 files changed, 538 insertions(+), 10 deletions(-)

## Rebase

Rebased onto origin/main (`d8497786`). One conflict in `cmd/herd/main.go` — HEAD added FAC-147 ClaimStack code, my branch added FAC-222 coordinator Resolve code. Both sides kept (different concerns). No API drift encountered. Second commit applied cleanly.

## Fixes applied (both BLOCKING)

### #1 — Inverted feedbackInterval formula

`feedback.CensusTickInterval(tickIntervalSeconds int)` extracted to `pkg/feedback/census.go` with correct formula `int(DefaultInterval.Seconds()) / tickSec` = 1800/15 = 120. Replaced inverted inline formula in `cmd/herd/main.go`. Non-vacuous test `TestCensusTickInterval` proven to fail on old formula (15s→1, want 120).

### #2 — coordinator.Register hardcodes the name

`Register` signature changed to `(root, name, workspace string)`. Caller-supplied name flows Register→file→Resolve→dispatch. Non-vacuous test `TestRegisterCustomNameFlowsThroughResolve` proven to fail on old hardcode (name="coordinator", want "forge-coordinator").

## Verification

- `go build ./...` — exit 0
- `go test ./pkg/feedback/ ./pkg/coordinator/ ./pkg/daemon/ ./pkg/dispatch/ -count=1` — all pass except `TestDispatch_PackageCwdNotPolluted` which is pre-existing flaky (confirmed fails on origin/main `d8497786` in a clean baseline worktree)
- `make lint` (go vet + hermeticity scanner) — exit 0

## INFORMATIONAL (out of scope, noted, left unchanged)

- #3: Resolve error silently swallowed by dispatch
- #4: No coordinator.json cleanup on loop exit
