sha: 52bbffb7dc79760923fd3650f66cbf086f0f957d
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: review-fac-765-52bbffb7dc79
reviewer-family: openai
verdict: FAIL
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: 52bbffb7dc79760923fd3650f66cbf086f0f957d
---
## Findings and risk

Reviewed exactly `9737258c9ecd7a48da215ea729cceff3cbc1d011..52bbffb7dc79760923fd3650f66cbf086f0f957d` as one R3 production range containing all six commits. The coordinator supplied the R3 classification and recorded primary author family as xai; W4 has no primary launch log, so I did not copy or invent a builder receipt or builder-family header. Reviewer family is openai, independent of the recorded xai author family.

Acceptance criteria checked: canonical accepted launch membership; receipt-file locator-only behavior; reviewer host/session binding; builder-only receipt refusal; tampered StartToken refusal; positive authentic membership; append-only and same-host idempotence; shared CLI flag/helper behavior; canonical reaching builder provenance; test-delta refusal; cross-host FAIL/BLOCKED dissent propagation through eligibility, queue, readiness, admission, bind, and completion.

1. **[High] A reachable production safety reader still collapses host-labelled dissent by reviewer name.** `pkg/review/ledger.go:164-175` implements `Vetoed` with a `SHA+reviewer` key and its `LedgerRow` has no Host field. Production `cmd/herd/pulse.go:626-633` and `cmd/herd/pulse.go:970-979` call this reader; `cmd/herd/pulse.go:952-955` uses its result to set `AwaitingVerdict` for committed lanes. A host-A FAIL followed by host-B PASS under the same reviewer therefore clears the veto in this path, even though the changed `pkg/reviewledger` consumers correctly retain it. An executable probe reproduced exactly that loss and exited 1. This can make pulse/reaping treat disputed committed work as not awaiting verdict, so the candidate is not safe to merge until this reachable reader is host-aware or is removed from the authority path.

The changed host-ingest and reviewledger paths otherwise behaved as intended in the focused checks. The legacy reader is an actual reachable mismatch, not merely an unreferenced duplicate: `go list -deps ./cmd/herd` includes `pkg/review`, and the two pulse call sites above invoke it.

Residual risk: `cmd/herd/candidate_cmd.go:72-102` also projects the legacy reader's host-less snapshot into one review per SHA, so operator-facing candidate output can hide cross-host dissent even though merge admission remains fail-closed in `pkg/reviewledger`.

## Tests run

- `code-review-graph detect-changes --repo <pool-root> --base 9737258c9ecd7a48da215ea729cceff3cbc1d011 --brief`: exit 0; 21 changed files, overall risk 0.60; graph later refreshed at candidate head with 126 nodes, 1194 edges, 9 files.
- `go test ./pkg/launch -run 'TestAcceptedCanonicalMember|TestAcceptedReviewLaunchFor|TestReachingBuilderReceipt' -count=1`: exit 0.
- `go test ./pkg/reviewledger -run 'TestHostIngest|TestMergeReadiness|TestBindEvidence|TestCompleteAdmissionRecord|TestQueued|TestPassSHAs|TestVetoSHAs' -count=1`: exit 0.
- `go test ./cmd/herd -run 'Test(ParseReviewHostIngest|ForgedReceipt|CanonicalReviewLaunch|HostIngest|TakeCLIFlag|ParseReviewBindEvidence|ResolveHarvestCandidate|NoNewDuplicatedRules)' -count=1`: exit 0.
- Temporary authenticated cross-host probe exercising `AdmitReduced`, `BindEvidence`, and `CompleteAdmissionRecord` after host-A FAIL plus host-B PASS: exit 0; all three retained the veto. Temporary test was removed.
- Mutation controls, each run in the pool and restored immediately: disable `Member` gate → targeted nonmember test exit 1; collapse projection to SHA+reviewer → cross-host dissent test exit 1; bypass canonical membership → forged-receipt test exit 1; disable StartToken gate → tampered-token test exit 1.
- Temporary executable probe of reachable `pkg/review.Vetoed` with host-A FAIL then host-B PASS: exit 1, printing that the legacy reader dropped the FAIL. Probe was removed.
- `go test ./pkg/review -count=1`: exit 1 at unrelated `TestPipelineContract_ConflictEvidenceIsRebaseNeeded` (`conflict=unknown`). No blind retry was run; this package result is recorded as a limitation, not attributed to FAC-765.

## Verdict

FAIL — the authenticated host-ingest and updated reviewledger consumers are focused-test green, but the reachable pulse/reaping reader still loses the exact cross-host dissent FAC-765 is required to preserve.
