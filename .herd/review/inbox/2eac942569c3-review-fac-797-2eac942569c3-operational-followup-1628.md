sha: 2eac942569c39f356404ad175d9991638d22217d
branch: recovery/mender-provenance-claude-1600
task: FAC-797
reviewer: review-fac-797-2eac942569c3
reviewer-family: open-weight
builder-family: anthropic
verdict: FAIL
reviewed-base: f214a3e9ebf10f178af9f0310a537c3637ce3e63
reviewed-head: 2eac942569c39f356404ad175d9991638d22217d
---

## Findings and risk

Operational-followup severity reassessment of the retained PASS. This artifact does not rewrite, replace, or amend the prior verdict: the original PASS artifact remains retained immutable at .herd/review/inbox/2eac942569c3-review-fac-797-2eac942569c3.md (sha256 990515014d518ea20b01dfa803e7dbcd5e8ec48aff7bd42932c2d914c25b4133, unchanged since ingestion export; root reviewed its native export, mail seq 2460, and retained it immutable while withholding admission). Same candidate, base, head, patch_id dba2b71b236f646d2dc1822f0d9b41fe4837aa04, verification_digest sha256:a728518ac6f8a2d9a1ca48be7e30968792ce519ba01cf3a6cf7ab192bff69f69, full_range f214a3e9ebf10f178af9f0310a537c3637ce3e63..2eac942569c39f356404ad175d9991638d22217d. No retry-of or reassesses prior-digest is claimed: the review process supplied no authenticated verdict-event digest for that header, and inventing one is forbidden.

Source of new acceptance context versus original packet: the original review packet (review-fac-797-2eac942569c3.md) contained ONLY the five narrow provenance acceptance criteria, the FAC-797 card scope, and review-isolation rules. It did NOT contain any overarching acceptance contract about how Herdforge itself must be forged. The new, now-authoritative context enters solely from this root directive: the user overarching acceptance contract requires Herdforge to forge itself through isolated native-fleet worktrees with worktree-local managed verify. Under the original packet alone, finding 1 was correctly judged non-blocking (the narrow criteria were literally satisfied). Under the newly stated contract, the same finding is release-blocking. Code behavior and test evidence are unchanged between the two artifacts; only the acceptance context and therefore severity change. Risk floor remains R2 (herd review-classify effective=R2, unchanged).

Severity reassessment — finding 1 escalates from non-blocking documentation/intent mismatch to RELEASE BLOCKER. Standing evidence from the retained PASS (independently executed, not re-derived here): in linked git worktrees (.git is a regular FILE, as in .worktrees/* and .herd/pool-*/*), Go's buildvcs stamping recognizes only .git DIRECTORIES (cmd/go/internal/vcs/vcs.go, vcsGit.RootNames isDir:true) and walks up to the canonical checkout, embedding vcs.revision=4074852990241458e8208b50e6821b1bff5362e7 (canonical HEAD, an ancestor of this candidate) with vcs.modified=true; scripts/build-herd.sh:28,37 stamps BinaryRevision=<worktree HEAD>; the new contradiction gate (pkg/provenance/provenance.go:324-326, enforced first in Validate at provenance.go:163-165) hard-rejects both-present-differ. Consequence: every bin/herd produced by `make build` inside ANY linked worktree fails provenance.Read/Validate/ValidateInstalled against its own worktree (self-gate paths cmd/herd/main.go:1218/1231 and 9243/9249/9253, gated by managedSelfGateExecutable), i.e., worktree-local managed verify is broken for worktree-built binaries fleet-wide.

Answer to the question posed: YES — release must hold until corrected build provenance lands. The narrow FAC-797 mismatch rule is met and must NOT be weakened: strict rejection of contradictory identity records is correct, wanted by the card, and independently RED/GREEN-proven. The defect is in build provenance for linked worktrees, not in the gate: the assigned fix (nativeFable, based on 2eac942569c39f356404ad175d9991638d22217d) must make worktree-produced binaries carry coherent, verifiable identity (e.g., worktree-aware stamping or suppression of Go's misattributed native stamp at build time — mechanism is the fix's scope, not prescribed here). Until that lands, Herdforge cannot satisfy the overarching contract of forging itself through isolated worktree lanes with worktree-local managed verify, so 2eac942569c39f356404ad175d9991638d22217d must not be harvested, merged, installed, or released.

Numbered findings:
1. (RELEASE BLOCKER — escalated from retained-PASS finding 1) Worktree-built stamped binaries are hard-rejected by their own worktree's provenance self-gates; breaks the user overarching acceptance contract (self-forging through isolated native-fleet worktrees + worktree-local managed verify). Correction required in build provenance before release; gate strictness itself is correct and retained.
2. (Unchanged, non-blocking) No dedicated in-process contradiction test; proven impossible to trigger under `go test` (test binaries carry no vcs settings); covered structurally via the shared comparison line proven load-bearing by the retained RED proof.
3. (Unchanged, disclosed) ValidateInstalled covered transitively via validateInstalledInfos -> Validate.

Residual risk: the operational blocker persists on all linked-worktree lanes until the assigned build-provenance fix lands and is re-reviewed; the retained PASS narrow-criteria evidence remains valid for the repair's base. I ran no new code tests for this follow-up (per root instruction; prior evidence stands); Linux integration gate and merge/install remain root-owned.

## Tests run

No code tests were re-executed for this follow-up, per root instruction that already-evidenced tests need not be rerun; the retained PASS artifact's Tests run section remains the standing verification evidence for the same SHA. Commands actually executed this turn:

- git rev-parse HEAD -> 2eac942569c39f356404ad175d9991638d22217d (clean pin confirmation; equals sha and reviewed-head). git rev-parse --show-toplevel -> /Users/kampe/Personal/Herdforge/.herd/pool-fac708-gemini-flash-0342/pool-01 (isolation preserved). git status --porcelain -> empty.
- sha256sum of retained original PASS artifact -> 990515014d518ea20b01dfa803e7dbcd5e8ec48aff7bd42932c2d914c25b4133 (unmodified, immutable preservation confirmed).
- Scratch cleanup verification (exact ownership, no-live-use): all four reviewer scratch paths (/tmp/probe-herd-bin throwaway probe binary, /tmp/red-fac797.out, /tmp/green-fac797.out, /tmp/provenance.go.reviewfac797.bak) were already deleted in the prior review turn and verified absent now (ls -> No such file or directory). The probe binary was never installed into any bin/ path, never executed as a live process, and never referenced by any running pane; its observations are preserved as digests/prose in the retained PASS artifact (vcs.revision=40748529..., vcs.modified=true evidence). The author's /tmp/provenance.go.fixed-1600 (dated 10:41, predating my lease) is not mine and was deliberately NOT touched. No logs or digests were deleted; no other cleanup performed.
- herd verdict-push --artifact of THIS follow-up artifact -> ledger ref/commit recorded in the push output; ordinary rootmail with this artifact's path, sha256 digest, actual verdict, and clean-pin confirmation sent to forge-orchestrator-39a9827d2b.
