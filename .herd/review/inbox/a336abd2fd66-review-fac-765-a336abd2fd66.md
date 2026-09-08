sha: a336abd2fd66b7ac2eda4b99c7a9bbb4d7c2d766
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: review-fac-765-a336abd2fd66
reviewer-family: openai
verdict: FAIL
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: a336abd2fd66b7ac2eda4b99c7a9bbb4d7c2d766
---

# Review verdict

FAIL for the complete seven-commit R3 production range
`9737258c9ecd7a48da215ea729cceff3cbc1d011..a336abd2fd66b7ac2eda4b99c7a9bbb4d7c2d766`.

## Findings

1. **High — reduced admission can fail open on authenticated cross-host dissent.**
   `pkg/reviewledger/admission.go:97-101` checks vetoes while iterating the
   projection-keyed `latest` map, but `:164` returns a successful PASS result
   from that same unordered loop. With an authenticated host-A FAIL or BLOCKED
   and host-B PASS (including both append orders), map iteration can visit PASS
   first and return `Admitted: true` before the veto is examined. This is a
   fail-open R3 merge-admission path. The full `Admit` path correctly performs
   the veto scan before selecting a PASS, but `pkg/mergeadmit/reconcile.go:216`
   uses `AdmitReduced` for legacy/reduced admission.

   The temporary proof exercised FAIL and BLOCKED, each in host-A-veto-then-
   host-B-PASS and reverse order, through eligibility, queue, readiness,
   reduced/full admission, bind, and complete. Eligibility, queue, readiness,
   full admission, bind, and complete refused dissent; reduced admission
   returned `Admitted: true` in the failing observations. The proof exited 1.

2. **Medium — candidate display nondeterministically selects conflicting host
   identity fields.** `cmd/herd/candidate_cmd.go:105-125` merges all host
   projections for a SHA by ranging over a map. When two authenticated host
   projections both carry vetoes but disagree on artifact/reviewer family, the
   later map entry overwrites the display fields through `enrichReview`. The
   resulting candidate display can alternate between the two identities across
   identical reads. `candidate.Resolve` sorts alternatives when there are
   multiple SHAs, but that does not stabilize conflicting fields for one SHA.
   The temporary 100-iteration proof observed both `a.md/anthropic` and
   `b.md/xai` and exited 1.

## Reviewed behavior and historical evidence

The range was reviewed as one production change across all seven commits, not
only the final pulse-reader commit. Canonical accepted launch membership,
receipt-file locator-only behavior, reviewer host/session binding, builder-only
receipt refusal, StartToken mismatch refusal, positive authentic membership,
append-only reassessment/idempotence, and dissent preservation were checked.
The canonical launch and host-ingest focused tests passed. Same-host
reassessment and legacy empty-host rows passed. The reachable
`pkg/review.Vetoed` path and its pulse `AwaitingVerdict` and candidate display
consumers passed the cross-host dissent tests. The candidate consumer preserves
a single cross-host FAIL over PASS and same-host reassessment clears the veto.

Historical evidence is retained: prior `a334` PASS, prior `3a` FAIL, and prior
`52bb` FAIL. The actual W4 `52bb` FAIL artifact is recorded by the coordinator
as `.herd/review/inbox/52bbffb7dc79-review-fac-765-52bbffb7dc79.md` with digest
`760454967dc0ec3a110c84e788c2423addb839ef6b8e8232608464d4cac5575b`.
That artifact is not present in this pool surface. The earlier W4
conflict-fixture failure remained a separate read-only diagnosis and was not
claimed as passed. The generated `docs/prompts/review-contract.md` path is
absent, consistent with FAC-668; the repository contracts used were
`.herd/prompts/reviewer.md` and `.herd/prompts/review-verdict.template.md`.

## Tests run

All commands used the verified host Go installation
`$HOME/.local/share/mise/installs/go/1.26.4/bin/go` (resolved as
`/home/kampe/.local/share/mise/installs/go/1.26.4/bin/go`). No full package
run was repeated after a failure.

- `go test ./pkg/launch -run 'TestAcceptedCanonicalMember|TestAcceptedReviewLaunchFor|TestReachingBuilderReceipt' -count=1` — exit 0.
- `go test ./pkg/reviewledger -run 'TestHostIngest|TestMergeReadiness|TestCompleteAdmissionRecord|TestBindEvidence|TestSameReviewerReassessment' -count=1` — exit 0.
- `go test ./pkg/review ./cmd/herd -run 'TestVetoedKeepsCrossHostDissent|TestLoadReapEvidenceCrossHostDissentKeepsAwaitingVerdict|TestLedgerReviewsAdmittedForRefPreservesCrossHostDissent|TestReadPulseReviewNamesRawVetoedSetExplicitly|TestApplyReapEvidenceAwaitingVerdictFlipIsNonVacuous' -count=1` — exit 0.
- Temporary `TestFAC765TemporaryHostLifecycleBothVetoKindsAndOrders` and `TestFAC765TemporaryCompleteDoesNotMixConflictingHostIdentities` — exit 1; the lifecycle assertion exposed reduced-admission fail-open behavior, while the conflicting completion identity assertion passed.
- Temporary `TestFAC765TemporaryAggregateDoesNotVaryAcrossConflictingHosts` — exit 1; observed both conflicting display identities.
- Mutation control: temporarily collapsed `pkg/review.Vetoed` projections to SHA-only and ran `go test ./pkg/review -run 'TestVetoedKeepsCrossHostDissent' -count=1` — exit 1 (expected RED). Restored the production projection and reran the focused veto/consumer checks — exit 0 (GREEN).
- `go test ./pkg/reviewledger -run 'TestSameReviewerReassessment|TestReassessmentRefusesIdentityAndEvidenceChanges|TestMergeReadiness_SameReviewerSupersedes|TestCompleteAdmissionRecordRefusesConflicts|TestBindEvidence_ConflictingBindingFailsClosed' -count=1` — exit 0.
- `go test ./cmd/herd -run 'TestLedgerReviewsAdmittedForRefPreservesCrossHostDissent' -count=1` — exit 0.
- `git diff --check` and pool `git status --porcelain=v1` after cleanup — exit 0; no source or temporary proof changes remain.

No builder-family field is asserted here because no primary W4 launch log is
locally provable. The primary canonical accepted fac-765-builder receipt is
not copied or invented. Native ingest should derive the builder family from
its canonical authority.
