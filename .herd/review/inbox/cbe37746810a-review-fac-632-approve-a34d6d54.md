sha: cbe37746810af7060a35691a8b0319139c36b21d
branch: fac-632-approve-alarm
task: FAC-632
reviewer: pool-03
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-head: cbe37746810af7060a35691a8b0319139c36b21d
---

Candidate-specific findings:

1. [High] `runApprove` does not alarm whenever an approval sweep closes nothing. `cmd/herd/main.go` computes `open := len(tasks) - approved`, but the alarm is gated only by `approved == 0 && failed > 0`. Honest `ErrNoEvidence` outcomes increment `refused`, and previously tombstoned cards increment `suppressed`; a sweep containing only either class therefore reports `approved=0`, leaves every card open, exits zero, and emits no control-plane stall alarm. The new CLI test covers only the hard-error path (`failed=1`), so it cannot detect the refused/suppressed zero-close cases named by the change.

2. [High] the new pulse board read fails open. In `cmd/herd/pulse.go`, an error from `tp.ListTasks(..., "in-review")` is ignored; `InReview` remains zero and `ProviderObservation.Known` is still set true. A timeout or provider failure can therefore be reported as `in_review=0`, bypass the new `>= 50` critical alarm, and leave dispatch eligible even though the in-review backlog is unknown. This contradicts pulse's fail-closed provider-error contract. No test exercises this collection/error path; the added pulse test injects a completed observation directly into `Plan`.

3. [Low] `cmd/herd/pulse.go` is not gofmt-clean: the added blank line contains a tab (`gofmt -d` reports the one-line whitespace diff).

Evidence:

- Isolation: Git toplevel was the leased `.herd/pool/pool-03` worktree; HEAD matched the packet SHA before and after review; final `git status --porcelain` was empty.
- `make preflight`: PASS (boundary, signal-literal, merge-policy, and main/origin drift checks passed; expected local fence-broker warning only).
- `go test ./pkg/pulse -run '^TestPlanInReviewStall$' -count=1`: PASS.
- `go test ./cmd/herd -run '^TestApproveCLI_StallAlarmFires$' -count=1`: PASS.
- Non-vacuity: restoring parent `cmd/herd/main.go` made `TestApproveCLI_StallAlarmFires` fail because the alarm was absent; restoring parent `pkg/pulse/pulse.go` made `TestPlanInReviewStall` fail to build because `InReview` was absent. Candidate files were restored and the tree rechecked clean.
- `go test ./cmd/herd ./pkg/pulse`: PASS.
- `make lint all`: incomplete. Vet, FAC-215 hermeticity, and package inventory passed, then the unchanged security gate failed because the installed `gosec` mise shim has no Go version configured and produced non-JSON consumed by `jq`. Running `make all` separately passed preflight, build, nested contract tests, and hermetic compilation; its shuffled full suite reported failures in unchanged packages `harness`, `herdr`, `review`, `sync`, and `verifier`. Changed-package suites passed independently.
- Code graph unavailable: the repository wrapper reported its pinned `code-review-graph` tool missing. Direct diff/source inspection was used as the documented fallback.
- `docs/prompts/review-contract.md` is absent from the candidate Git tree, so it could not be read without violating the packet's candidate-only/isolation boundary.
