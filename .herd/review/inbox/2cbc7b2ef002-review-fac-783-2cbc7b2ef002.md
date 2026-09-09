sha: 2cbc7b2ef002097a9df38e52e468c489a1b256e3
branch: recovery/fac-783-flash-auth-repair
task: FAC-783
reviewer: review-fac-783-2cbc7b2ef002
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 3278e8965e3548b41938c990c9f0c11f91a2e249
reviewed-head: 2cbc7b2ef002097a9df38e52e468c489a1b256e3
---

## Review

task_ref: FAC-783
candidate_sha: 2cbc7b2ef002097a9df38e52e468c489a1b256e3
patch_id: full two-commit range 3278e8965e3548b41938c990c9f0c11f91a2e249..2cbc7b2ef002097a9df38e52e468c489a1b256e3
risk_tier: R3
verdict: FAIL
author_family: open-weight
reviewer_family: openai
verification_digest: graph built_at_commit=2cbc7b2ef002097a9df38e52e468c489a1b256e3; focused and race checks passed; stale-already-done negative control reproduced exit 1
reviewed_at: 2026-09-09T17:54:24Z

The full range addresses the four prior findings for normal in-progress tasks: receipt authentication precedes provider lookup, full receipts use exact task reads without ListTasks, missing full task identity refuses before provider calls, exact provider ref/project identity is required, and full/reduced/idempotence/readback tests pass. One R3 authorization gap remains.

## Findings

1. [High] `pkg/sync/boarddone.go:538-543` skips the live provider-revision comparison whenever the exact task read reports `done`. With no matching done-log receipt digest, a validly sealed but stale full receipt is accepted solely because terminal status is treated as sufficient. `BoardDoneFenced` then returns its already-done success at `pkg/sync/boarddone.go:733-738` after only the live-lease check; the fenced production path used by `approveOne` can therefore approve stale evidence, and this path does not append the missing done-log receipt. The required revision gate is bypassed for exactly the terminal-state replay the packet calls out. A temporary compiling control with a stale full receipt, an already-done exact task, and no done-log record failed as expected against a fail-closed implementation but failed on this candidate (the candidate returned nil; command exit 1): `go test ./pkg/sync -run '^TestFAC783ReviewControl_StaleReceiptOnAlreadyDoneTaskRefuses$' -count=1`. Preserve the revision binding for terminal tasks and make crash recovery depend on receipt-bound durable evidence rather than a generic done status.

## Residual risk

The candidate remains fail-closed for tampered receipts, missing full task IDs, partial/mismatched exact provider identities, stale revisions on non-terminal tasks, reduced-receipt compatibility, provider readback drift, and ordinary repeated receipt delivery. The terminal-task exception remains merge-blocking for this R3 change.

## Tests run

- `go version` — exit 0 (`go1.26.6 darwin/arm64`)
- `go test ./pkg/sync -count=1` — exit 0
- `go test ./cmd/herd -count=1` — exit 0
- `go test -race ./pkg/sync -count=1` — exit 0
- `go build ./cmd/herd` — exit 0
- `go vet ./pkg/sync ./cmd/herd` — exit 0
- `go run ./scripts/hermeticity/` — exit 0
- `make preflight` — exit 0
- `make lint` — exit 0
- `go test ./pkg/sync -run '^TestFAC783ReviewControl_StaleReceiptOnAlreadyDoneTaskRefuses$' -count=1` — exit 1 (intentional negative control reproduced the bypass)
- `git status --short --branch` after cleanup — clean, exit 0
