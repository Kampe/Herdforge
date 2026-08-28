sha: a84ca0ce6ea1d9d21cbb8fd4cf39f006f86dd2b6
branch: fac-632-approve-alarm
task: FAC-632
reviewer: review-fac-632-a84ca0ce6ea1
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-head: a84ca0ce6ea1d9d21cbb8fd4cf39f006f86dd2b6
---

# Review evidence

## Blocking finding

- [HIGH] The new pulse alarm counts every open GitHub issue as an in-review card and can stop a healthy GitHub-backed fleet. `readPulseProvider` calls `ListTasks(project, "in-review")` and assigns `len(inReviewTasks)` directly to `ProviderObservation.InReview` (`cmd/herd/pulse.go:259`). The GitHub adapter maps every status other than done/archived to the GitHub `state=open` query, returns the resulting issues without post-filtering (`pkg/provider/github.go:147` and `pkg/provider/github.go:198`), and maps those open issues to canonical `to-do` (`pkg/provider/github.go:66`). Consequently, a repository with 50 open to-do issues and no review work is observed as `InReview=50`; `Plan` then sets `UnknownCritical`, blocks dispatch, and exits non-zero (`pkg/pulse/pulse.go:595` and `pkg/pulse/pulse.go:1012`). An isolated temporary regression test confirmed `GitHubProvider.ListTasks(..., StatusInReview)` returned all 50 open to-do issues. This is a false control-plane stall on a supported task provider and must be corrected before merge.

## Structural review

- Reviewed the complete FAC-632 branch delta from merge base `8cc98285303681fc3aa386fbc38fbd26edd2368a` through candidate HEAD: five files, 103 insertions, one deletion.
- `code-review-graph` was refreshed at the candidate SHA: 16,465 nodes, 204,672 edges, 1,391 files. It reported eight changed symbols, no affected named flows, and risk 0.60. Direct source inspection confirmed the affected production paths are `runApprove`, `readPulseProvider`, `ProviderObservation`, `Plan`, and `FormatHuman`.

## Verification

- `make preflight`: PASS. Boundary, path-leak, signal-literal, merge-policy, and main/origin-main checks passed.
- Targeted candidate tests: PASS.
  - `go test ./cmd/herd -run 'TestApproveCLI_StallAlarmFires($|OnSuppressed$)' -count=1`
  - `go test ./pkg/pulse -run 'TestPlanInReviewStall$' -count=1`
- Non-vacuity probes: PASS. Omitting `suppressed` from the approve stall count made `TestApproveCLI_StallAlarmFiresOnSuppressed` fail. Moving the pulse threshold from `>= 50` to `> 50` made `TestPlanInReviewStall` fail. Both files were restored and the targeted tests passed again.
- `make lint all`: BLOCKED by host tooling before the full test gate. The configured `gosec` mise shim has no selected runtime, emits no JSON, and the security gate's `jq` aggregation fails on null. The candidate does not modify this tooling.
- `make all`: build, nested contract tests, hermetic compilation, and the changed packages (`cmd/herd`, `pkg/provider`, `pkg/pulse`) passed. The overall shuffled unit suite failed in unrelated packages (`pkg/harness`, `pkg/herdr`, `pkg/launch`, `pkg/review`, `pkg/store`, `pkg/sync`, and `pkg/verifier`) with timeout/environment/concurrency-sensitive failures; none are touched by this candidate.
- Final candidate tree: clean at `a84ca0ce6ea1d9d21cbb8fd4cf39f006f86dd2b6`.

## Review limitation

- The packet-required `docs/prompts/review-contract.md` is absent from the candidate, its parent, `origin/main`, and repository history available in this review surface. The review therefore used the packet and repository `AGENTS.md` contract as authority.
