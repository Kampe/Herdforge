sha: 3c4c3f08cdf7d1c80c86384dc863decaaa606f88
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: review-fac-765-3c4c3f08cdf7
reviewer-family: openai
builder-family: xai
verdict: FAIL
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: 3c4c3f08cdf7d1c80c86384dc863decaaa606f88
---
## Findings and risk

This is the complete independent R3 review of all thirteen commits in the
range `9737258c9ecd7a48da215ea729cceff3cbc1d011..3c4c3f08cdf7d1c80c86384dc863decaaa606f88`.
The worktree HEAD exactly matches the reviewed head. The author family is the
accepted `xai` provenance from the read-only primary evidence report; the
reviewer family is `openai`, so the families are independent. Native patch ID
is `b9f81550c34ad33d8f7ad56ce74d17430b9bb69d`. This is a new full-range
artifact and intentionally has no `reassesses` field. The prior admitted FAIL
for candidate `66257f36` remains historical and unchanged, as does the first
BLOCKED artifact for this candidate.

The candidate fixes the previously reported VetoSHAs same-host retry
projection and the focused positive, cross-host, replacement, revocation,
candidate binding, branch harvest, and native launch-locator tests pass. The
R3 acceptance contract is nevertheless not met:

1. **High — fabricated PASS RetryOf clears a real veto without an authenticated
   retry projection.** `pkg/reviewledger/operations.go:468-476` emits a
   supersession audit whenever a PASS merely supplies `RetryOf`, without
   proving the named reviewer has a same-host record or accepted receipt.
   `pkg/reviewledger/host_ingest.go:86-98` and
   `pkg/review/ledger.go:174-205,1127-1174` then derive retry authority from
   any latest PASS row with a non-empty `RetryOf`. A temporary non-vacuous RED
   fixture recorded a valid host-A FAIL, appended only a fabricated host-A
   PASS RetryOf row for reviewer-b (no reviewer-b record or launch), and
   observed `Vetoed` erase the valid veto. This violates the explicit
   requirement that retry authority cannot come from a fabricated row or a
   non-empty Reviewer field alone, and affects final veto/eligibility and
   merge-facing projections.

2. **High — legacy closure ignores the native queue-only revocation.** The
   native FAIL/BLOCKED producer writes `EventRevoked` to the harvest queue at
   `pkg/reviewledger/operations.go:504-518`. The legacy closure consumer at
   `cmd/herd/legacy_review_authority.go:51-76` scans only `snap.Rows`, never
   `snap.Queue`, before accepting a PASS. A temporary native producer/consumer
   RED fixture used the real `Record` and `Verdict` APIs to create a PASS then
   a same-host FAIL; the queue contained the expected native revocation, yet
   `AdmittedPass` still returned the earlier PASS. The existing synthetic
   main-ledger revocation test does not cover this native queue path, so this
   is a real stale-closure authorization failure.

3. **High — admission-record completion is not host-scoped.**
   `pkg/reviewledger/complete_record.go:34-60` stores one `prior` record per
   SHA/reviewer, then selects a PASS from `latest` by reviewer only. The
   `ProjectionKey` host is ignored in both selections, and the public method
   accepts only task, SHA, and reviewer. A temporary RED fixture placed the
   host-B record before the host-A record and supplied only a host-B PASS; the
   method returned success and accepted the host-B evidence for the host-A
   record. This violates exact canonical member/host binding and can append a
   completion record using a separately resolved launch projection.

These are blocking findings for R3 authentication, provenance, and destructive
admission behavior. The branch-scoped harvest and candidate display positives
do not compensate for the final-consumer failures above. Do not merge or admit
this candidate until each fix has a native producer-to-consumer regression,
including rejection of fabricated retry rows and exact host selection.

The supplied builder-family evidence is only a read-only primary provenance
report, not a launch receipt and not a source for a fabricated receipt. The
coordinator integration log is also separate evidence: its `make ci` passed at
integration head `d44bcf04e601ee013a9867354863a2a652270e74`, with SHA256
`8ce2fb24377abe45bf42fe119b060cd13cd9ae46438202f87319e815b9708fac`, but it
is not a managed verification receipt for this source candidate.

## Tests run

- `code-review-graph build --repo .`: completed full graph build (17,719 nodes,
  226,798 edges, 1,529 files). `detect-changes` for the pinned base reported
  37 changed files, 177 changed symbols, overall R3 risk 0.85, and test gaps;
  callers and affected flows were then checked against source.
- `make preflight`: PASS.
- Focused native reviewer/host-ingest, launch-locator, review-ledger,
  attention, legacy-closure, harvest, branch, merge-readiness, and projection
  tests: PASS. This included `TestVerdictProjectionsShareRetryAuthority`,
  `TestGenericSupersessionDoesNotClearCurrentVeto`,
  `TestSameHostRetryReachesAttentionPipeline`,
  `TestSameHostRetryReachesLegacyClosure`,
  `TestLegacyClosureEventRevokedWithdrawsSHA`,
  `TestLegacyClosureIdentityReplacementWithdrawsPrevious`,
  `TestRecordStartedCopiesReviewerDecisionCandidateSHA`,
  `TestRecordStartedDoesNotInventBuilderCandidateSHA`, and the pinned
  candidate/task/host/family mismatch refusals.
- Guard-removal proof: after removing the retry-audit and self-identity guards
  in `IdentityReplacementSHA`, one combined run of the three retained
  regressions produced RED: `TestGenericSupersessionDoesNotClearCurrentVeto`
  (same-host positive subtest), `TestSameHostRetryReachesAttentionPipeline`,
  and `TestSameHostRetryReachesLegacyClosure` failed. The guards were restored;
  the identical combined command returned GREEN for both packages.
- Temporary non-vacuous RED proofs, all restored/deleted afterward:
  `TestFAC765FabricatedRetryCannotClearVeto` failed because `Vetoed` cleared a
  valid veto from a fabricated retry; `TestFAC765NativeRevocationWithdrawsLegacyClosure`
  failed because native queue revocation did not withdraw legacy PASS; and
  `TestFAC765CompletionRequiresExactHostProjection` failed because host-B PASS
  completed host-A. No temporary files remain.
- `git diff --check`: PASS; final worktree source is clean. The preserved first
  artifact remains byte-identical with SHA256
  `939be88853aaabda6de67ac97c5b9f19c486e368b6e8e4b14dcbeb70bcf79bf4`.
- Prior broad local limitations remain separate evidence and were not rerun:
  `make lint all` stopped at the known security-gate `jq` null failure, and
  `make all` retained environment-only hook/host-tool failures. The separate
  coordinator `make ci` result above is not this reviewer's candidate receipt.

Reviewed at `2026-09-08`; actual verdict is FAIL.
