sha: a73b430bdf76fecf843e26794196babc100ca940
branch: recovery/fac-628-flash-drift
task: FAC-628
reviewer: review-fac-628-a73b430bdf76
reviewer-family: openai
builder-family: open-weight
verdict: BLOCKED
reviewed-base: 93d9c2ba42034d18938adcb28d584790adc0dd91
reviewed-head: a73b430bdf76fecf843e26794196babc100ca940
---

patch_id: not supplied in the review packet
verification_digest: not supplied in the review packet
risk_tier: R3
reviewed_at: 2026-09-09T18:26:38Z

## Verdict

BLOCKED. The exact candidate and cross-family boundary are proven, but the packet omits the patch ID and verification digest required by the reviewer admission gate. This review grants no merge authority. The code review also found a fail-closed provenance defect below.

## Findings

1. [Blocker] The packet does not include the required `patch_id` or `verification_digest`. The reviewer contract requires both before a verdict can be validly admitted; neither can be invented or imported from the prior candidate.

2. [High] `cmd/herd/spin.go:368-416` accepts incomplete accepted launch provenance as a known identity. The worktree check only rejects a mismatch when both the receipt and live pane provide `CWD`, and the latest-receipt selection does not require `CreatedAt`. Therefore a matching tab/pane receipt with missing live worktree, or an accepted receipt with zero timestamp, returns `known=true` and can emit a false `MODEL_DRIFT`/`FORBIDDEN` result. Temporary compiling probes reproduced both cases with exit 1 and were removed. Incomplete provenance must remain unknown.

3. [High] `cmd/herd/spin.go:137-143` silently discards missing or malformed `goal-guard.json` errors and leaves `Progress.Continuations` at zero. The new `pkg/spin` logic then treats `StateChangeSeq` churn as progress, so the empty continuation loop the feature is meant to detect can remain `PROGRESSING` when its durable counter cannot be read. This loses the fail-closed distinction and can leave a quota-burning lane unnudged.

## Graph evidence

- Exact range verified as `93d9c2ba42034d18938adcb28d584790adc0dd91..a73b430bdf76fecf843e26794196babc100ca940`; base is an ancestor of the candidate.
- `code-review-graph detect-changes --base 93d9c2ba42034d18938adcb28d584790adc0dd91 --brief`: 7 changed files, 20 changed functions, 11 test gaps, risk score 0.35, 0 affected flows.
- Refreshed graph status: 84 nodes, 847 edges, 4 indexed Go files, built at the reviewed head.
- Graph tracing confirmed `runSpin` reaches `emitSpin` and `spinDriftReport`, `spinDriftReport` reaches `spinModelDrift`, and `runRaise` applies `ForbiddenLaunchModel` before inventory and launch side effects.

## Tests run

- `go version` — exit 0 (`go1.26.6 darwin/arm64`)
- `go test -count=1 ./cmd/herd -run 'TestSpinModelDrift|TestSpinDriftReport|TestEmitSpin'` — exit 0
- `go test -count=1 ./pkg/spin -run 'Test(EmptyContinuationLoopIsNotProgress|LongLegitToolWorkIsNotCalledAnEmptyLoop|RealWorkAcrossAContinuationBoundaryIsProgress|ModelDriftNeedsBothSidesAndFiresOnDifference)'` — exit 0
- `go test -count=1 ./pkg/standing -run 'Test(ForbiddenLaunchModel|RaiseRefusesForbiddenModelBeforeAnySideEffect)'` — exit 0
- `go test -race -count=1 ./cmd/herd -run 'TestSpinModelDrift|TestSpinDriftReport|TestEmitSpin'` — exit 0
- `go test -race -count=1 ./pkg/spin ./pkg/standing` — exit 0
- `go build ./cmd/herd` — exit 0
- `go vet ./cmd/herd ./pkg/spin ./pkg/standing` — exit 0
- `go run ./scripts/hermeticity/` — exit 0
- `make preflight` — exit 0; emitted expected stale unembedded-binary and fenced-board warnings, with boundary, path-leak, signal-literal, merge-policy, and main/origin checks passing.
- `git diff --check` — exit 0
- Mutation: removed continuation-churn suppression; `go test -count=1 ./pkg/spin -run '^TestEmptyContinuationLoopIsNotProgress$'` — exit 1 as expected (`PROGRESSING`)
- Mutation: disabled forbidden-model refusal; `go test -count=1 ./pkg/standing -run '^Test(ForbiddenLaunchModelRefusesGrokThatIsNot46|RaiseRefusesForbiddenModelBeforeAnySideEffect)$'` — exit 1 as expected
- Mutation: disabled tab/pane and worktree identity checks; `go test -count=1 ./cmd/herd -run '^TestSpinModelDrift(.*)$'` — exit 1 as expected
- Missing-live-worktree probe — `go test -count=1 ./cmd/herd -run '^TestFAC628ProbeMissingLiveWorktreeMustStayUnknown$'` — exit 1, reproducing `known=true drifted=true`
- Missing-receipt-timestamp probe — `go test -count=1 ./cmd/herd -run '^TestFAC628ProbeMissingReceiptTimestampMustStayUnknown$'` — exit 1, reproducing `known=true drifted=true`
- Broader `go test -count=1 -timeout=600s ./cmd/herd ./pkg/spin ./pkg/standing` — not completed: the `cmd/herd` integration test process hung without output and was terminated by the reviewer; no exit code was observed.

## Residual risk

Focused candidate tests and relevant local checks pass, and the prior f12 identity/JSON findings are covered by the new tests. The incomplete-provenance and goal-guard read-error paths remain unresolved. Coordinator-owned clean WSL `make ci` was not duplicated.
