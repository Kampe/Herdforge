sha: ee06413f2af7caa1ae5d6d3cb2df57c9f147efa4
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: review-fac-765-ee06413f2af7
reviewer-family: openai
builder-family: xai
verdict: PASS
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: ee06413f2af7caa1ae5d6d3cb2df57c9f147efa4
---

Reviewed the exact 15-commit range `9737258c9ecd7a48da215ea729cceff3cbc1d011..ee06413f2af7caa1ae5d6d3cb2df57c9f147efa4` as one R3 candidate. The candidate is the exact reviewed HEAD, is an ancestor of the recorded branch `repair/fac-765-authenticated-host-ingest`, and has no tracked source changes in the isolated worktree. The builder family is independently recorded as xai; this review is performed by the different openai family.

Acceptance criteria were met. Legacy admission now retains host-specific cross-host FAIL/BLOCKED dissent in either append order, while valid same-host retries supersede only their exact host projection. Queue revoke and SHA-wide consumption semantics remain append-only and deterministic. Candidate retry display and admission share `RetrySupersessionFromLatest`; fabricated RetryOf references without current and prior native records do not authorize harvest. Native host ingest requires the exact candidate SHA, reviewer identity, host/session, canonical accepted REVIEWER launch receipt, reaching builder provenance, and allowlisted family evidence. Receipt, family, digest, host, replay, and branch mismatches are rejected. Candidate and harvest projections retain unsuperseded host-specific dissent, and harvest selects the latest exact branch-bound record when available.

Findings: none. I found no blocking, high, medium, or low production defect in the reviewed range. The pending coordinator integration checks were not represented as source-test results and were not used to manufacture evidence; the native focused gates below provide the review decision for this exact candidate.

Mutation proof was performed against the live candidate and reverted before verification. Disabling the legacy host-veto guard made `TestLegacyClosureCrossHostDissentBothOrders` fail in all four order/host cases. Disabling the candidate supersession guard made the valid same-host retry positive test fail because the stale FAIL projection was displayed instead of the retry PASS. The fabricated-retry consumer test still passed under that latter isolated mutation because `coherentReviews` independently preserves the veto; the native reviewledger fabricated-retry coverage remains intact, so this is defense-in-depth rather than a vacuous assertion. No skipped assertion was used.

Tests run:

- `git diff --check` — PASS.
- `env -u GOROOT go test ./cmd/herd -run 'Test(LegacyClosureCrossHostDissentBothOrders|LedgerReviewsSameHostRetryRetainsPass|CandidateFabricatedRetryDoesNotAuthorizeHarvest|HostLabelledHarvestSelectsExactBranchBoundPass|ForgedReceiptRejectedByHostIngest|NativeStartedReviewReceiptAuthenticatesHostIngest)' -count=1` — PASS.
- `env -u GOROOT go test ./pkg/reviewledger -run 'Test(SameHostRetry|CrossHostRetry|EligibleCrossHost|EligibleSameHost|FabricatedRetry|HostIngest|CompletionRequiresExactHostProjection)' -count=1` — PASS.
- `env -u GOROOT go test ./pkg/review -run 'Test(VerdictProjections|VetoedKeepsCrossHostDissent)' -count=1` — PASS.
- `env -u GOROOT go test ./pkg/launch -run 'Test(RecordStarted|AcceptedCanonicalMember|AcceptedReviewLaunchFor|ReachingBuilderReceipt)' -count=1` — PASS.
- `env -u GOROOT go test ./pkg/mergeadmit -run 'Test(ReconcileLandedReducedRefusesAuthenticatedCrossHostDissent|NoMergeAdmissionBypass)' -count=1` — PASS.
- The broader focused native set across `pkg/launch`, `pkg/reviewledger`, `pkg/review`, `pkg/mergeadmit`, and `cmd/herd` also passed.

The complete command/output record is retained in `.herd/fac765-review.log`. Its verification digest is `sha256:f3832ce999cadb218f631d6d50dea7b5f4e6f42c48948df9987b22577c964a96`.

Reviewed at `2026-09-08T15:36:23Z`. The only worktree status entry is the intentional untracked surface-local review log; tracked source remains clean.
