sha: 5ba3eff7a0cfce492824124db01589177e3c56d1
branch: fac-632-approve-alarm
task: FAC-632
reviewer: review-fac-632-approve-c13e4ba3
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-head: 5ba3eff7a0cfce492824124db01589177e3c56d1
---

[P1] Add a non-vacuous guard for refusal and suppression stalls — cmd/herd/main.go:3228

The new `stalled` calculation broadens the alarm from `failed` to `failed + refused + suppressed`, but the only stall-alarm test does not exercise either newly covered path. After replacing only `cmd/herd/main.go` with its `HEAD~1` version, `go test ./cmd/herd -run '^TestApproveCLI_StallAlarmFires$' -count=1` still passed with exit 0. The fixture therefore proves only the pre-existing `failed > 0` behavior, so reverting this candidate to the failed-only condition remains green and the refusal/suppression one-way-valve regression is unguarded. This violates the repository's hard non-vacuity requirement for approval-path negative assertions.

Overall assessment: the two production corrections are directionally fail-closed, but the approval correction cannot be approved without a regression guard that fails against the pre-candidate implementation.

Evidence:

- Reviewed HEAD was exactly `5ba3eff7a0cfce492824124db01589177e3c56d1`; the review pool was clean before and after the mutation check.
- Candidate targeted tests passed: `TestApproveCLI_StallAlarmFires` and `TestPlanInReviewStall`.
- Non-vacuity mutation failed its gate: the stall-alarm test still passed after restoring `cmd/herd/main.go` to `HEAD~1` (`mutation-exit=0`).
- Package verification passed: `go test ./cmd/herd ./pkg/pulse -count=1` (`cmd/herd` 100.758s; `pkg/pulse` 0.008s).
- Residual test gap: the corrected `readPulseProvider` error propagation has no direct adapter-level test for an `in-review` `ListTasks` failure.
