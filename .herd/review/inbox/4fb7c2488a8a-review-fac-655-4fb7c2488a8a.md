sha: 4fb7c2488a8a93eff99fcbfb1780a44112e14783
branch: recovery/fac-655-record-completion
task: FAC-655
reviewer: review-fac-655-4fb7c2488a8a
reviewer-family: openai
verdict: PASS
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: 4fb7c2488a8a93eff99fcbfb1780a44112e14783
---
## Findings and risk

Reviewed the complete range `9737258c9ecd7a48da215ea729cceff3cbc1d011..4fb7c2488a8a93eff99fcbfb1780a44112e14783`, including both `cc6063099db4e9f3ed434a34d36fc4475c7411e` and `4fb7c2488a8a93eff99fcbfb1780a44112e14783`; this is not a parent-delta-only review. The classifier independently retained the explicit R3 floor. Patch ID: `23ce024c689e5079960ebc1b648a886d7de4cd03`.

Acceptance criteria were authenticated completion of an existing pool-placeholder record without changing its exact candidate/reviewer/family/task identity, preserving lease and patch bindings, refusing conflicting or dissenting evidence, remaining append-only and idempotent, and isolating lock fixtures from inherited canonical-root/lock environment while preserving explicit overrides. No finding remains against those criteria.

1. `Ingest` now completes the already-admitted placeholder under the existing ledger/inode lock before appending the verdict. The merge inherits authenticated verdict families, binds the closeable card only when the retained branch proves a legacy branch task, preserves existing lease/patch fields, rejects conflicts/dissent, and is idempotent. The direct completion path exercises the same merge and retains append-only history.
2. The lock fixture strips inherited lock-scope variables but appends explicit test overrides. The new inherited-root assertion is non-vacuous: restoring the pre-fix `os.Environ()` implementation made it fail by placing the lock in the foreign root; the candidate implementation passes, and the explicit override test also passes.

Residual risk: the independent full `cmd/herd` package run reported one failure in unchanged `cmd/herd/landed_observation_test.go` (`TestObserveVerifyLandedSquashPreservesCandidate`), outside the six changed files and unrelated to the changed reviewledger/lock-fixture paths. `pkg/reviewledger` passed its full package run. The supplied native full-suite receipt `458abd2e...` for the exact new head is external evidence and is reported separately; it is not represented as an independently executed full-suite result here. Builder family is intentionally omitted because it was not locally provable on W4; this verdict does not re-derive or assert it. The original `cc6063099db4e9f3ed434a34d36fc4475c7411e` managed FAIL is not being rewritten by this review.

Verification digest (derived from the Tests run section): `25e735b1ad1ca37386ec748ca87953e5`.

## Tests run

- `/home/kampe/.local/share/mise/installs/go/1.26.4/bin/go version` with that directory first in PATH — `go version go1.26.4 linux/amd64`.
- `go run ./cmd/herd review-classify recovery/fac-655-record-completion --tier R3 --pin 4fb7c2488a8a93eff99fcbfb1780a44112e14783 --json` — PASS; effective tier R3 and required high-risk gates reported.
- `go test ./pkg/reviewledger -run 'Test(CompleteAdmissionRecord|CompletingAnUnrecordedRecordPreservesItsGate|IngestCompletesLegacyBranchPlaceholderWithoutDroppingLease|CompleteLaunchProvenance)' -count=1` — PASS.
- `go test ./cmd/herd -run 'Test(ReviewCompleteRecord|ReviewIngestCompletesPoolPlaceholder|LockWithChildSeesEnvHeld|LockIgnoresInheritedCanonicalRoot|LockEnvOverrides)' -count=1` — PASS.
- `go test ./pkg/reviewledger ./cmd/herd -count=1` — `pkg/reviewledger` PASS; `cmd/herd` FAIL only at unchanged `TestObserveVerifyLandedSquashPreservesCandidate`.
- `go test -race ./pkg/reviewledger -count=1` — PASS.
- `go test -race ./cmd/herd -run 'Test(ReviewCompleteRecord|ReviewIngestCompletesPoolPlaceholder|LockWithChildSeesEnvHeld|LockIgnoresInheritedCanonicalRoot|LockEnvOverrides)' -count=1` — PASS.
- `go vet ./pkg/reviewledger ./cmd/herd` — PASS.
- `git diff --check 9737258c9ecd7a48da215ea729cceff3cbc1d011..4fb7c2488a8a93eff99fcbfb1780a44112e14783` — PASS.
- RED mutation proof: temporarily restored `cmd/herd/lock_test.go` to `cmd.Env = append(os.Environ(), env...)`; `TestLockIgnoresInheritedCanonicalRoot` FAILed with foreign-root redirection, then the tracked file was restored and the candidate test passed.
- RED mutation proof: temporarily removed the new `completeAuthenticatedLaunchRecord` call from `pkg/reviewledger/operations.go`; `TestIngestCompletesLegacyBranchPlaceholderWithoutDroppingLease` FAILed with the uncompleted `recovery/fac-655-record-completion`/`unrecorded` row, then the tracked file was restored.
- Final worktree identity after all temporary proofs: top-level `/home/kampe/Projects/Herdforge/.herd/pool-fac655-4fb7/pool-01`, `HEAD=4fb7c2488a8a93eff99fcbfb1780a44112e14783`, clean status.

## Author instructions

None; PASS.
