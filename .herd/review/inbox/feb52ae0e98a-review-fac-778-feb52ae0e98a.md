sha: feb52ae0e98ac67661b27185decd156edb79fd92
branch: recovery/fac-778-branch-publication
task: FAC-778
reviewer: review-fac-778-feb52ae0e98a
reviewer-family: openai
builder-family: xai
verdict: BLOCKED
reviewed-base: dba61c240e3ebb35bd3a065c491e0ce2584c8328
reviewed-head: feb52ae0e98ac67661b27185decd156edb79fd92
---
## Findings and risk

task_ref: FAC-778
candidate_sha: feb52ae0e98ac67661b27185decd156edb79fd92
patch_id: a6d4db5d6d3f8fc7b68448441fec9a5b941e7120
risk_tier: R3
verdict: BLOCKED
author_family: xai
reviewer_family: openai
verification_digest: derived by review ingest from the executed evidence under `## Tests run`
reviewed_at: 2026-09-08T19:02:47Z

The exact eight-file range `dba61c240e3ebb35bd3a065c491e0ce2584c8328..feb52ae0e98ac67661b27185decd156edb79fd92` was reviewed as one production-and-test unit. `git patch-id --stable` reproduced packet patch ID `a6d4db5d6d3f8fc7b68448441fec9a5b941e7120`; HEAD and the sole reaching remote branch matched the packet. The local code-review graph was pinned to the reviewed head, detected all eight changed files and 27 changed symbols, and focused caller/source inspection confirmed `Dispatcher.Dispatch` is the sole production caller of `buildTaskPacket`.

No source-level correctness or security defect was found in the candidate. The R3 gates were assessed as follows:

1. The enum admits only omitted/empty, `coordinator-harvest`, and `lane-push`; loaded configuration wraps invalid values as `merge_policy.branch_publication` failures.
2. Omitted policy and explicit `coordinator-harvest` preserve the incumbent no-push packet bytes. `lane-push` alone emits push instructions.
3. The lane-push packet uses the branch returned by the worktree result and signed task context, not `herd/<ref>` or a project slug. Unsafe shell characters are single-quote escaped. The packet requires remote-head equality with committed HEAD before handoff and continues to forbid worker PR creation, merging, and root-checkout mutation.
4. `branchPublicationMode` is called in `Dispatcher.Dispatch` after read-only task/lane resolution but before ownership acquisition, worktree creation, board mutation, artifact publication, and Herdr launch. The tests invoke the real `Dispatcher.Dispatch` producer. Deliberately bypassing validation made the config and dispatcher rejection tests fail; disabling the lane-push switch made both lane-push packet tests fail.

Blocking condition B1: the repository's mandatory security-capable lint gate could not execute its pinned `gosec` scanner on this host. `gosec` resolves to a mise shim with no configured Go version; the direct command exits non-zero with the exact error recorded below. `make lint` consequently exits non-zero in `scripts/security-gate.zsh` after vet, nested vet, hermeticity, and package inventory have passed. Per the packet's fail-closed instruction, this unavailable toolchain component is BLOCKED and grants no merge approval. No PATH, mise, source, or toolchain configuration was changed.

The repository-wide shuffled unit command also exited non-zero in unrelated host-sensitive packages (`pkg/harness`, `pkg/launch`, `pkg/security`, `pkg/store`, and `pkg/verifier`), while both changed packages passed in that same run. A serial rerun made `pkg/store` pass but retained loopback-hook, host-PATH isolation, and hermetic-Go lookup failures. These failures do not map to the eight-file candidate diff, but they are recorded as failed checks rather than represented as passing evidence. The packet's stated coordinator run with a pinned external parity fixture was not executed or independently observed here.

Residual risk: the changed-package and mutation evidence is strong, but the unavailable security scanner and red repository-wide host gates prevent a valid independent PASS for this R3 candidate. Re-run the exact candidate in the configured/pinned verification environment and issue a fresh independent verdict only after the security and repository gates produce admissible results.

## Tests run

- `git diff --check dba61c240e3ebb35bd3a065c491e0ce2584c8328..feb52ae0e98ac67661b27185decd156edb79fd92` — exit 0; no whitespace errors.
- `git patch-id --stable < <(git diff dba61c240e3ebb35bd3a065c491e0ce2584c8328..feb52ae0e98ac67661b27185decd156edb79fd92)` — exit 0; produced `a6d4db5d6d3f8fc7b68448441fec9a5b941e7120`.
- `go run ./cmd/herd review-classify origin/recovery/fac-778-branch-publication --pin feb52ae0e98ac67661b27185decd156edb79fd92 --json` — exit 0; inferred/effective tier R3 for all eight changed paths.
- `go vet ./pkg/config ./pkg/dispatch` — exit 0.
- `go test -count=1 -run 'TestDispatchPacket|TestDispatchRejectsInvalidBranchPublication' -v ./pkg/dispatch` — exit 0; all five publication/rejection tests passed.
- `go test -count=1 ./pkg/dispatch` — exit 0; package passed.
- `go test -race -count=1 ./pkg/dispatch` — exit 0; package passed under the race detector.
- After temporarily replacing `ValidateBranchPublication` with an accept-all review mutant, `go test -count=1 -run 'TestMergePolicyBranchPublicationAuthority|TestMergePolicyValidateBranchPublication|TestDispatchRejectsInvalidBranchPublicationBeforeLaunch' ./pkg/config ./pkg/dispatch` — exit 1 as required; all invalid-mode config cases and the production dispatcher rejection test failed. The file was restored immediately.
- After temporarily making the lane-push switch case unreachable, `go test -count=1 -run 'TestDispatchPacketLanePushUsesAssignedBranchAndForbidsPRMerge|TestDispatchPacketLanePushEscapesUnsafeAssignedBranch' ./pkg/dispatch` — exit 1 as required; both lane-push packet tests failed. The file was restored immediately.
- `go test -count=1 -run 'TestMergePolicyBranchPublicationAuthority|TestMergePolicyValidateBranchPublication' ./pkg/config` followed by `go test -count=1 -run 'TestDispatchPacket|TestDispatchRejectsInvalidBranchPublication' ./pkg/dispatch` — both exit 0 after restoration.
- `make preflight` — exit 2. Workspace boundary, signal-literal, and merge-policy checks passed; the aggregate command refused an unstamped temporary `go run` binary as `herd provenance: STALE`.
- `go test -count=1 -shuffle=on -timeout=300s ./...` — exit 1. Changed packages `pkg/config` and `pkg/dispatch` passed; unrelated failures occurred in `pkg/harness`, `pkg/launch`, `pkg/security`, `pkg/store`, and `pkg/verifier`.
- `go test -p 1 -count=1 ./pkg/harness ./pkg/launch ./pkg/security ./pkg/store ./pkg/verifier` — exit 1. `pkg/store` passed serially; the remaining unrelated failures reproduced as loopback hook timeouts, host `PATH` isolation refusals, and missing Go in the hermetic verifier PATH.
- `make lint` — exit 2. Root and nested `go vet`, the FAC-215 hermeticity scan, and the FAC-301 package inventory passed; `scripts/security-gate.zsh` exited non-zero at the security step.
- `gosec ./pkg/config ./pkg/dispatch` — exit 1 with exact tool error: `mise ERROR No version is set for shim: gosec` and guidance to set a global Go version. No PATH or toolchain change was attempted.

Final integrity check: `git status --porcelain` was empty and `git rev-parse HEAD` remained `feb52ae0e98ac67661b27185decd156edb79fd92` after both mutation proofs and all verification commands.

## Author instructions

No source correction is requested. The candidate requires a fresh security-capable review or verification run on a host where the pinned `gosec` command and the repository's configured gates are usable, with the resulting exact-head evidence retained for admission.
