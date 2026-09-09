sha: bedd645985e3e7b101e8a2b1d7f04fd6864b6321
branch: recovery/fac-740-flash-retention
task: FAC-740
reviewer: review-fac-740-bedd645985e3
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 93d9c2ba42034d18938adcb28d584790adc0dd91
reviewed-head: bedd645985e3e7b101e8a2b1d7f04fd6864b6321
---

task_ref: FAC-740
candidate_sha: bedd645985e3e7b101e8a2b1d7f04fd6864b6321
patch_id: a05b4a193e182dce9b683dae8f3d5af7285872a2
risk_tier: R3
verdict: FAIL
author_family: open-weight
reviewer_family: openai
verification_digest: sha256:a97fcae377e872f8b7338a51aa64fec621fc0852e54bb65c3ebda28d2e54b096
reviewed_at: 2026-09-09T12:55:32-05:00

## Summary

The candidate correctly authenticates the signed effect and binds task, candidate SHA, base SHA, lease, generation, intent/delivered state, and supersession in the exercised paths. The exact public broker callback path and the native `review-ingest --recover-verdict` path both pass their supplied positive and negative tests. The following R3 failures remain.

## Findings

1. [Blocker] A retention failure can permanently strand a delivered verdict while the retry reports success. The new retention call is after provider delivery/readback and before the consumable callback (`cmd/herd/main.go:9665-9684`). If retention fails, the provider already contains the effect and the provider claim marker remains, but no delivered callback exists. On retry, `winVerdictClaim` creates a fresh random owner on every call and returns `owned=false` for the prior claim (`cmd/herd/main.go:10088-10096`); the caller then returns `OK` without retrying retention or publishing the callback (`cmd/herd/main.go:9610-9614`). I reproduced this through `serveBrokerConn`: pre-seeding a conflicting inbox destination made the first attempt fail, and the retry returned `OK` with zero delivered callbacks. This violates the stated crash/retry convergence and can again leave the provider verdict with no canonical review authority.

2. [High] Recovery accepts reviewer identity and verification evidence that are not bound to the coordinator-signed effect. `VerdictRecoveryRecord` takes `Reviewer`, `ReviewerFamily`, `Verification`, and `VerificationDigest` from the recovery JSON (`pkg/reviewingest/verdict_recovery.go:180-212`). Recovery only requires a non-empty family, rejects known coordinator names, and checks branch reach (`pkg/reviewingest/verdict_recovery.go:308-355`); retention checks only that a supplied digest equals a digest recomputed from the supplied verification text (`pkg/reviewingest/verdict_recovery.go:297-305`). The signed effect preimage is only the effect ID plus the broker line, and that line contains no reviewer or verification binding. A caller holding a publicly readable signed provider effect can therefore materialize it as an arbitrary non-coordinator reviewer/family with arbitrary self-authored verification evidence. This weakens receipt ownership and verification-digest binding and lets recovery manufacture independent-review metadata around an otherwise valid effect.

3. [High] Concurrent retention can overwrite conflicting evidence despite the documented collision refusal. Both `retainVerdictBytes` (`pkg/reviewingest/verdict_recovery.go:370-390`) and the existing `RetainArtifact` path use a check-then-publish sequence, while `publishRetainedArtifact` publishes with `os.Rename(tmpName, dst)` (`pkg/reviewingest/retain.go:161-185`). On POSIX, rename replaces an existing destination. Two concurrent recovery calls for the same effect but different reviewer/verification records can both observe an absent destination, both return success, and let the last rename replace the first artifact. The promised “conflicting retry ... is refused” invariant is therefore not atomic, and review evidence can silently change under concurrent recovery.

## Tests run

- `go version` — exit 0 (`go1.26.6 darwin/arm64`).
- `go test -count=1 ./pkg/reviewingest ./pkg/mail ./cmd/herd` — exit 0.
- `go test -count=1 ./pkg/reviewingest -run 'Test(RetainVerdictArtifact|RecoverVerdictArtifact|ParseVerdictEffect|LedgerVerdict)'` — exit 0.
- `go test -count=1 ./cmd/herd -run 'Test(BrokerVerdict|RecoverVerdict)'` — exit 0.
- `go test -race -count=1 ./pkg/reviewingest ./pkg/mail` — exit 0.
- `go test -race -count=1 ./cmd/herd -run '^(TestBrokerVerdict_RetainsCanonicalInboxArtifact|TestBrokerVerdict_RetryAfterArtifactDelete_HealsCrashWindow|TestRecoverVerdictMaterializesDeliveredEffectsForAdmission|TestRecoverVerdictRefusesIntentOnlyAndSupersededEvidence|TestVerdict_TwoBrokersDeliverExactlyOnce)$'` — exit 0.
- `go build -o /tmp/herd-fac740-review ./cmd/herd` — exit 0.
- `go vet ./pkg/reviewingest ./pkg/mail ./cmd/herd` — exit 0.
- `go run ./scripts/hermeticity/` — exit 0.
- Mutation control: temporarily disabled the effect-signature gate, then ran `go test -count=1 ./pkg/reviewingest -run '^TestRetainVerdictArtifact_RefusesUnsignedEffect$'` — exit 1 (expected RED); restored the candidate and reran the same test — exit 0.
- Crash-window reproduction: temporary untracked `TestFAC740TmpRetentionFailureRetry` drove the real `serveBrokerConn` path; first retention failed, retry returned `OK`, and delivered callback count remained zero — test command exit 0 because it asserted that observed failure. Temporary test removed.
- `git diff --check 93d9c2ba42034d18938adcb28d584790adc0dd91 bedd645985e3e7b101e8a2b1d7f04fd6864b6321` — exit 0; final `git status --short` clean.

## Residual risk

Do not merge this revision until retry ownership/convergence, recovery provenance binding, and atomic no-replace retention are corrected and covered by non-vacuous tests. The coordinator should treat the current candidate as lacking merge authority despite the green package, race, build, vet, and hermeticity checks.
