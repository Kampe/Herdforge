sha: feb52ae0e98ac67661b27185decd156edb79fd92
branch: recovery/fac-778-branch-publication
task: FAC-778
reviewer: review-fac-778-feb52ae0e98a
reviewer-family: openai
builder-family: xai
verdict: PASS
reviewed-base: dba61c240e3ebb35bd3a065c491e0ce2584c8328
reviewed-head: feb52ae0e98ac67661b27185decd156edb79fd92
reassesses: 8246df059c0b04e17731699bd6b60cb996fbbbdfa65918ea33f74ccb8272ef92
---
## Findings and risk

task_ref: FAC-778
candidate_sha: feb52ae0e98ac67661b27185decd156edb79fd92
patch_id: a6d4db5d6d3f8fc7b68448441fec9a5b941e7120
risk_tier: R3
verdict: PASS
author_family: xai
reviewer_family: openai
verification_digest: derived by review ingest from the new executed evidence under `## Tests run`
reviewed_at: 2026-09-08T19:18:24Z

This is an authenticated same-reviewer reassessment of admitted BLOCKED verdict event `8246df059c0b04e17731699bd6b60cb996fbbbdfa65918ea33f74ccb8272ef92`, not a rewrite. The original artifact remains unchanged at SHA-256 `75e150d3cab54070582a51f7f5e93bd7aaaf57e0bf68b56bed40a10eeb048f92`. Its full-range source analysis, changed-package verification, race run, and two RED/GREEN mutation proofs remain the review basis for exact range `dba61c240e3ebb35bd3a065c491e0ce2584c8328..feb52ae0e98ac67661b27185decd156edb79fd92`.

Prior blocking condition B1 is resolved. The existing scanner at `/home/kampe/.local/share/mise/installs/go/1.26.2/bin/gosec` matched authorized SHA-256 `9d29b491d0851d5852b81aa214c534f9f936ada62964384bcef8829059522842`, executed successfully, and embeds `github.com/securego/gosec/v2 v2.27.1`. A temporary command-local resolver exposed only this scanner; `go` continued to resolve through the existing reviewer shim and reported Go 1.26.4. The unchanged native `scripts/security-gate.zsh` passed both the FAC-251 gosec HIGH/CRITICAL baseline and gitleaks history baseline. No package, global PATH/profile, mise setting, scanner policy, baseline, exclusion, deadline, source file, or other reviewer's environment was changed; the temporary resolver was removed.

Coordinator-executed evidence was inspected separately and is not represented as reviewer-executed verification. `fac778-feb5-mac-ci-evidence.json` binds integration `c6d932b0367da1ddcc795eef85a11a3cf3dc265e` to this reviewed candidate/base and identical source patch `a6d4db5d6d3f8fc7b68448441fec9a5b941e7120`. Its attached 250-line log matched recorded SHA-256 `a4133db64df1e4d41720afb822815970ff21ef42442a4dc4d33b9e019b03ffd6`, shows the exact candidate merged into that integration, records passing native security, dependency security, pinned 52-entry external parity, full unit, race, and preflight stages, and ends `CI gate PASSED`.

The original WSL broad-suite failures remain recorded in the admitted BLOCKED artifact. They were outside the eight-file range and are superseded as merge-gate evidence by the coordinator's exact-patch pinned integration run; they are not relabeled as passing reviewer commands. With B1 now executed successfully and the exact-patch integration evidence passing, no blocking correctness, security, authority, default-behavior, or test-non-vacuity finding remains for this candidate.

Residual risk: lane-push remains an explicit opt-in control-plane mode whose remote availability is environment-dependent. The candidate limits worker authority to publishing its assigned branch, retains coordinator-only PR/merge/root mutation authority, quotes unsafe branch characters, and preserves harvest-only behavior by default. No live provider operation or remote push was performed during review.

## Tests run

- `shasum -a 256 /home/kampe/.local/share/mise/installs/go/1.26.2/bin/gosec` — exit 0; matched `9d29b491d0851d5852b81aa214c534f9f936ada62964384bcef8829059522842`.
- `/home/kampe/.local/share/mise/installs/go/1.26.2/bin/gosec --version` — exit 0; scanner executed successfully.
- `go version -m /home/kampe/.local/share/mise/installs/go/1.26.2/bin/gosec` — exit 0; binary Go version `go1.26.2`, path `github.com/securego/gosec/v2/cmd/gosec`, module `github.com/securego/gosec/v2 v2.27.1`.
- `go version` and `command -v go` before the gate — exit 0; reviewer compiler remained `go1.26.4 linux/amd64` at the existing mise shim.
- Created a temporary one-entry resolver containing only a symlink to the verified scanner, then ran command-scoped `PATH="$fac778_scanner_bin:$PATH" zsh -c 'go version; shasum -a 256 "$commands[gosec]"; gosec --version; ./scripts/security-gate.zsh'` — exit 0. The in-command compiler remained Go 1.26.4, scanner hash remained exact, the gosec baseline passed, and the gitleaks baseline passed.
- `test ! -e /tmp/fac778-gosec.uC8YJ2` — exit 0; the temporary resolver was removed.
- `shasum -a 256 /home/kampe/Projects/Herdforge/.herd/review-packets/fac778-feb5-mac-ci.log` — exit 0; matched `a4133db64df1e4d41720afb822815970ff21ef42442a4dc4d33b9e019b03ffd6`. This verifies attachment integrity only; the Mac `make ci` was coordinator-executed evidence.
- `git status --porcelain`, `git rev-parse HEAD`, and `git patch-id --stable < <(git diff dba61c240e3ebb35bd3a065c491e0ce2584c8328..feb52ae0e98ac67661b27185decd156edb79fd92)` — exit 0; source tree clean, HEAD exact, patch ID `a6d4db5d6d3f8fc7b68448441fec9a5b941e7120`.

## Author instructions

No source correction is requested. The prior blocker was host scanner resolution rather than candidate code and is resolved by the newly executed native security gate.
