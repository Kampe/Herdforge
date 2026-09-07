sha: 7767a0b613d8449f36f4722cbd43356664d8f1bf
branch: herd/fac-631
task: FAC-631
task-id: rm82b4ojm6cpb8ls3id0lp95
reviewer: review-fac-631-r2-grok-7767a0b
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-base: e08b2227821d642c24608a46312eb776aeb6b8ec
reviewed-head: 7767a0b613d8449f36f4722cbd43356664d8f1bf
retry-of: review-fac-631-r1-grok-7767a0b
---

## Findings

No findings.

Independent read of the three-commit/two-file range
`e08b2227821d642c24608a46312eb776aeb6b8ec..7767a0b613d8449f36f4722cbd43356664d8f1bf`
(`./cmd/herd/reviewingest.go`, `./cmd/herd/reviewingest_tier_test.go`).

The shipped ingest path now classifies the candidate before writing the durable
record row, using `classify.Classify` on `diffStat(riskBase, a.SHA)` where
`riskBase` is the artifact `reviewed-base` (`a.ReadBase`) and only falls back
to `origin/main` when that field is empty. The record row carries
`RecordOpts.Tier`, which `reviewledger.AdmitReduced` requires (`l.Tier(sha)`
empty → skip with "no risk tier is recorded") and which
`mergeadmit.Gate.ReconcileLanded` copies onto the completion receipt as
`RiskTier`.

The follow-up commit is the load-bearing one: classifying against mutable
`origin/main` would under-tier a mixed control-plane/docs range after main
advanced past the R3 file. Binding to `a.ReadBase` keeps the reviewed range
stable. Fail-closed on `diffStat` error (`classify candidate risk`). Same
three-dot `base...target` contract as `review-classify`.

Graph (`code-review-graph` 2.3.7, `built_at_commit=7767a0b613d8449f36f4722cbd43356664d8f1bf`):
detect-changes reports two files, overall risk 0.55, and a static "untested
`runReviewIngest`" note. That gap is not real: the new test execs the shipped
`review-ingest` binary and asserts both the ledger record tier and
`ReconcileLanded` receipt `RiskTier`.

Residual, not a finding: `ValidatePassDiff` still diffs `origin/main...sha`
(pre-existing empty-PASS gate, unchanged here). RETIRED artifacts now also
hit `diffStat` even though retirement does not persist a tier; common clones
have `origin/main`, and a missing range refuses rather than forging a tier.

## Verification

Go: `go version go1.26.4 linux/amd64` via the assigned mise shim. Worktree
clean, detached at the candidate.

- `git rev-parse HEAD` → `7767a0b613d8449f36f4722cbd43356664d8f1bf` (exit 0)
- `git status --porcelain` → empty (exit 0)
- `git merge-base e08b2227821d642c24608a46312eb776aeb6b8ec 7767a0b613d8449f36f4722cbd43356664d8f1bf` → `e08b2227821d642c24608a46312eb776aeb6b8ec` (exit 0)
- `git diff --name-status` on that range → `M cmd/herd/reviewingest.go`, `A cmd/herd/reviewingest_tier_test.go` (exit 0)
- `git log --oneline` on that range →
  `323169d62 chore: anchor FAC-631 worktree (FAC-106 reap-safe)` (empty),
  `0550681fc fix: record ingest risk tier for reconciliation FAC-631`,
  `7767a0b61 fix: bind ingest tier to reviewed base FAC-631`
- `git diff --check` (worktree) exit 0; `git diff --check <base> <head>` exit 0
- `go test ./cmd/herd -run TestReviewIngestRiskTierReachesReceiptReconcile -count=1 -v` → PASS, exit 0
  - `documentation` PASS (R0)
  - `control_plane` PASS (R3)
  - `reviewed_base_survives_origin_main_advance` PASS (R3)
- `go test ./cmd/herd -run TestReviewIngest -count=1` → ok, exit 0
- `go test ./pkg/reviewledger -count=1` → ok, 0.116s, exit 0
- `go test ./pkg/mergeadmit -run ReconcileLanded -count=1` → ok, 0.179s, exit 0
- `go test ./pkg/classify -run TestClassify_GoldenVectors -count=1` → ok, exit 0
- `go build ./...` → exit 0
- `go run ./scripts/hermeticity/` → exit 0

Non-vacuity (independent of the test binary): `classify.Classify` on the
fixture path sets yields `docs/fac-631.md` → R0, `cmd/herd/fac631.go` → R3,
reviewed-base mixed `{cmd/herd/fac631-risk.go, docs/fac-631-followup.md}` → R3,
and origin/main-after-advance `{docs/fac-631-followup.md}` → R0. Those two
tiers differ, so a shipped classifier still using `origin/main` would record
R0 on the third case and fail `ingested record tier` / `completion receipt
tier`. Deleting `RecordOpts.Tier` would leave `gotTier` empty and fail the
same assertions. The test also fatals if the fixture does not expose that
mutable-base split.

## Scope/readback

Reviewed HEAD equals `sha` and `reviewed-head`:
`7767a0b613d8449f36f4722cbd43356664d8f1bf`. Reviewed base equals the
immutable pin `e08b2227821d642c24608a46312eb776aeb6b8ec`. Working tree stayed
clean; this verdict is the only write, and the refused r1 artifact was not
opened or edited.

Header keys compared to `./.herd/prompts/review-verdict.template.md` and to
`Artifact.Validate`'s accepted-key list in `./pkg/reviewingest/reviewingest.go`.
Template sample names `sha`, `branch`, `task`, `reviewer`, `reviewer-family`,
`builder-family`, `verdict`, `reviewed-head`, `retry-of`. The parser also
accepts `task-id` and `reviewed-base` (and refuses any other key). This
artifact uses exactly the packet's accepted set, no opening delimiter, no extra
key, `reviewed-head` equals `sha`, families differ (`xai` reviewer / `openai`
builder).
