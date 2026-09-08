---
sha: e0b47e7aef700270a390aeba5d3b12be79af189e
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: review-fac-765-e0b47e7aef70
reviewer-family: openai
builder-family: unrecorded
verdict: FAIL
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: e0b47e7aef700270a390aeba5d3b12be79af189e
---

## Findings and risk

Reviewed the complete nine-commit range `9737258c9ecd7a48da215ea729cceff3cbc1d011..e0b47e7aef700270a390aeba5d3b12be79af189e` as one R3 production change. The candidate is correctly pinned and the graph was refreshed at the exact head (9,146 nodes, 121,677 edges, 762 files). The acceptance criteria require candidate/task/repository/lane-bound canonical reviewer membership, exact host/session/member/start-token refusal, preservation of pre-edit builder receipt behavior, append-only cross-host dissent, deterministic display, and fail-closed eligibility/queue/readiness/admission/reconcile consumers.

1. [Blocker] Public HostIngest does not require an exact task/repository/lane identity tuple. `cmd/herd/review_host_ingest.go:183` calls `AcceptedReviewLaunchForCandidate` with the artifact task but empty repository and lane filters. `pkg/launch/receipts_canonical.go:123-136` also treats an empty member field as a wildcard whenever a filter is supplied. An accepted reviewer receipt for the requested SHA can therefore omit task/repository/lane (or carry a contradictory repository/lane that the CLI never checks) and still authenticate. A temporary public-path proof with blank `TaskRef`, `Repository`, and `Lane` was accepted and enqueued (`go test` exit 1 because the test expected refusal). This leaves host-ingest able to authenticate the wrong task/repository/lane despite the new candidate pin.

2. [High] Cross-host retry supersession remains keyed only by reviewer, so a retry on host B can erase dissent from host A in eligibility and queue selection. `pkg/reviewledger/operations.go:782-806` builds `supersededReviewers map[string]bool` and tests only `verdict.Reviewer`; the same reviewer-only map is repeated in `:920-979` for queued candidates. A temporary proof with host-A FAIL and host-B PASS carrying `RetryOf: reviewer-a` returned `Eligible=true, err=nil` (test exit 1 because the expected retained veto was absent). This violates the required cross-host retained FAIL/BLOCKED invariant and can send a dissenting candidate into harvest even though later merge admission remains more conservative.

The exact candidate-pinning guard is non-vacuous: replacing the new lookup with the prior name-only lookup made `TestCanonicalReviewProvenanceRejectsDifferentCandidate` fail (exit 1); restoring it made the cross-candidate refusal, canonical positive path, builder-pane refusal, and pre-edit empty-CandidateSHA positive path pass. The remaining residual risk is that the current green regression suite does not cover missing/contradictory identity fields or host-scoped retry supersession. No builder-family assertion is made here; the packet records `builder-family: unrecorded`.

## Tests run

- `command -v git` -> `/home/kampe/.local/state/herdforge-tools/git-2.55.0-complete/bin/git`; `git --version` -> `git version 2.55.0`.
- `command -v go` -> `/home/kampe/.local/share/mise/shims/go`; `go version` -> `go1.26.4 linux/amd64`. Test commands used the installed host binary `/home/kampe/.local/share/mise/installs/go/1.26.4/bin/go`.
- `.../go test -count=1 ./pkg/reviewledger` -> exit 0.
- `.../go test -count=1 ./pkg/review` -> exit 0.
- `.../go test -count=1 ./pkg/mergeadmit` -> exit 0.
- `.../go test -count=1 ./cmd/herd` -> exit 0 (127.974s).
- `.../go test -count=1 ./pkg/launch` -> exit 1. Fresh-environment failures were `TestOrdinaryRequestCannotBypassProductionDiscovery` (`hook.timeout`), `TestClaudeCommandIncidentRequiresBoundHealthPolicyBeforeEffects` (hook timeout/digest attribution), and `TestValidateOptionalHookWarningIsDeduplicatedAndIdentityPreserved` (timeout warning); these are unrelated hook-environment failures, not prior W4 Git2.34 evidence.
- `.../go test -count=1 ./pkg/launch -run 'TestAccepted|TestReachingBuilderReceipt'` -> exit 0.
- `.../go test -count=1 ./cmd/herd -run 'Test(ParseReviewHostIngest|ForgedReceipt|CanonicalReview|HostIngest|CanonicalReviewProvenance)'` -> exit 0.
- `.../go test -count=1 ./pkg/reviewledger -run 'TestHostIngest|TestAdmitReduced|Test.*Dissent|Test.*Readiness'` -> exit 0.
- `.../go test -count=1 ./pkg/mergeadmit -run 'TestReconcileLandedReducedRefusesAuthenticatedCrossHostDissent'` -> exit 0.
- Guard-removal RED/GREEN: temporary name-only lookup mutation made the cross-candidate regression exit 1; restored candidate-pinned lookup plus positive/negative focused tests exited 0. Temporary proof files were removed and the pool is clean.
- Temporary adversarial proofs: missing task/repository/lane public HostIngest was wrongly accepted (exit 1); host-B retry wrongly cleared host-A dissent in `Eligible` (exit 1). Both proof files were removed.
- `git status --porcelain=v1`, `git diff --check`, exact HEAD and base ancestry checks -> exit 0 / clean.
