sha: aea658d727b5ea7616296fdb506016c44b8455db
branch: herd/fac-649
task: FAC-649
reviewer: reviewer-claude-fac649
reviewer-family: anthropic
builder-family: openai
verdict: FAIL
reviewed-head: aea658d727b5ea7616296fdb506016c44b8455db
---
Risk tier: R3 (core lifecycle/infrastructure — executes configured external hook
binaries and mutates the Kaneo task board on behalf of automated agents).

Scope reviewed: pkg/lifecycle/lifecycle.go plus act_hold_test.go, hold_test.go,
lifecycle_bin_test.go, and the new lifecycle_fac649_test.go, at exact candidate
aea658d727b5ea7616296fdb506016c44b8455db on branch herd/fac-649.

What the change does well (verified by reading, not just claimed): it correctly
fail-closes the STALE-RECLAIM action. `approvedReclaimHook()` now requires
`ReclaimHook` to be a non-empty member of an explicit `ApprovedReclaimHooks`
allowlist before any board-mutating command runs (empty allowlist denies all).
`parseCards`/`rejectLifecycleErrorEnvelope` reject empty/non-array payloads,
`{"error":...}`/`{"errors":...}` envelopes, missing id/ref/status, and
duplicate id/ref — replacing the old silent multi-shape fallback parser.
`readCommand` now bounds every Kaneo/readback exec with a real context
timeout (default 30s) instead of an unbounded `exec.Command(...).Output()`.
Most importantly, in `executeActMode`'s reclaim block, a readback command
error is now a hard `ErrReclaimReadback` failure — previously the code used
`if err == nil && !verify(...)`, which silently treated a failed readback as
success. `lifecycle_fac649_test.go` exercises all of this (hook-not-approved,
timeout, error-envelope, duplicate id/ref, wrong ref/status/still-assigned)
and the fixtures/tests are internally consistent with the production code.

FAIL reason — the fix is incomplete relative to its own stated scope ("fail
closed Kaneo actions", plural) and leaves the identical bug class live in a
sibling action path this same commit makes more likely to trigger:

`executeActMode`'s ROUTING-REPAIR action (pkg/lifecycle/lifecycle.go, the
"blocked-only queue" branch, ~line 1517-1571) still has the exact fail-open
pattern this commit just removed from reclaim:

    readbackData, err := e.authoritativeReadback("route", blockedRefs,
        replacementRef, routeLane, routeOwner)
    if err == nil && !e.verifyRoutingReadback(readbackData, replacementRef,
        routeLane, routeOwner) {
        return fmt.Errorf("routing readback did not prove the exact
            replacement/lane/owner")
    }
    s.Actions = append(s.Actions, ActionLogEntry{
        Action: "routing-repair", ..., Verified: true,
    })
    s.RoutingActionExecuted = true

If `authoritativeReadback` returns a non-nil error (readback command fails,
provider unreachable, or — now more likely — the very `readCommand` timeout
this diff just introduced fires), the `err == nil && ...` guard short-circuits
to false, so the function does NOT return an error. It falls straight through
to appending `ActionLogEntry{..., Verified: true}` and setting
`s.RoutingActionExecuted = true`, recording an unverified routing-repair
action as proven. This is the same bug shape as the pre-fix reclaim path,
just left in place one function away.

Two concrete gaps versus the reclaim hardening applied elsewhere in this same
commit:
1. No allowlist equivalent to `ApprovedReclaimHooks` gates `e.RoutingHook` —
   any non-empty `RoutingHook` runs unconditionally (line ~1517-1525).
2. `verifyRoutingReadback`/`checkRoutingCard` (~line 1961-2115) is untouched:
   it still does substring `strings.Contains` status matching, tries a long
   chain of legacy field fallbacks (Column/State/Key/AssignedTo/AssignedAgent/
   AssignedLane), and never calls `rejectLifecycleErrorEnvelope`, unlike the
   new strict `validateReclaimReadback`.

Verified there is zero test coverage for this path: `grep -rn
"RoutingHook|routing-repair|routing readback|verifyRoutingReadback"
pkg/lifecycle/*_test.go` returns no matches. The new lifecycle_fac649_test.go
covers only the reclaim/board-list/timeout paths, not routing-repair.

Correction needed before merge: apply the same treatment already given to
reclaim — split the routing readback error from the verification-failed case
(hard `return` on `err != nil` from `authoritativeReadback("route", ...)`),
add an explicit `ApprovedRoutingHooks`-style allowlist gate for `RoutingHook`
mirroring `approvedReclaimHook()`, and add a `lifecycle_fac649_test.go` case
that proves a failing/timed-out routing readback is a hard failure, not a
recorded `Verified: true` action, matching the reclaim test pattern
(`TestApprovedReclaimHookAndReadbackFailuresAreHard`).

Build/test note: this reviewer's pool worktree (pool-01) has no `go` binary
and no `mise` on PATH, so `go build ./...` / `go test ./pkg/lifecycle/...`
could not be executed here; this verdict is based on full reading of the
diff, the unchanged surrounding code, and confirming (via git show/grep only,
no writes) that the flagged fail-open branch and its missing test coverage
are real and present at the reviewed HEAD.

Isolation note: this review was performed entirely inside the leased pool
worktree (/home/kampe/.local/state/herdforge-review/pool/pool-01, confirmed
via `git rev-parse --show-toplevel`). No file in the reviewed tree was
modified; `git status --porcelain` is clean at HEAD
aea658d727b5ea7616296fdb506016c44b8455db. An earlier read-only check
(`ls`/`grep`, no writes) briefly touched the canonical Herdforge checkout
solely to confirm `herd verdict-push` is a real, tested subcommand before
trusting the packet's report-home instruction; a user scope-guard message
mid-review asked that further inspection stay pool-only, which was honored
for the remainder of the review.
