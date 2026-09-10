sha: 6cf5dfad9deb6524414f78b2c53a1745edfa0f57
branch: recovery/fac-791-opencode-auth
task: FAC-791
reviewer: review-fac-791-6cf5dfad9deb
reviewer-family: openai
builder-family: open-weight
verdict: BLOCKED
reviewed-base: ba8e444c711f53d2861d833642ee685374aa8b54
reviewed-head: 6cf5dfad9deb6524414f78b2c53a1745edfa0f57
reassesses: ea4305e95f713690c7c10bb4933ce66ac39784c55f4926d09032ad767780d73f
---
## Findings and risk

This is an immutable same-reviewer reassessment of the preserved and ACKed prior BLOCKED event `ead3e063942e3837e00bb3e4e56004a71d776d770598e395c382d145dab614fc`. It assesses the exact unchanged FAC-791 candidate `ba8e444c711f53d2861d833642ee685374aa8b54..6cf5dfad9deb6524414f78b2c53a1745edfa0f57`; the prior artifact is not overwritten or reversed. Risk floor remains R3.

The prior clean-WSL gate is resolved by coordinator report1884: full unit, core race, vet, security, nested contracts, and preflight passed for the exact candidate, with raw-log readback digest `d64ec2e18caf7d761033f5fbf61a34cdec380238f9c340b1ac9603e75d4c3a82` recorded at `.worktrees/orchestrator/.herd/coordinator-resume/completion-recovery-20260909/wsl-ci-readback-0338.jsonl`.

The native launch evidence also resolves the FAC-791 source-security concern: the exact OpenCode Lazer Gemini3.7Flash route resolved, the secondary provider request succeeded, native auth classification/preflight did not refuse, and the real reviewer was created. This confirms the source review’s functional findings remain clear. No signed managed verifier is required by the card, and none is being claimed.

1. [Blocker] FAC-791 still cannot receive PASS or card-closure authority because live exact review-packet consumption was not achieved. The native `pkg/herdr.Send` call timed out while the reviewer’s last status was `working`; the exact reviewer tab was then closed by failure cleanup. This is a live delivery/consumption gate, not a FAC-791 source-auth defect: `pkg/herdr.Send` is unchanged by FAC-791 and FAC-792 owns the delivery defect. The card explicitly requires live packet consumption before closure, so the reassessment verdict remains BLOCKED until a later exact-candidate run proves consumption.

2. Nit: the prior source review’s `gofmt -d` check remains an alignment-only whitespace failure at `pkg/security/harness_auth.go:28`; source is unchanged, so this finding is preserved rather than re-tested or edited.

Residual risk is limited to the unproven live handoff/consumption boundary and the carried formatting nit. The successful native request proves provider reachability and the successful cleanup proves the failed launch was retired, but neither proves that the exact packet was consumed. Prior evidence and this reassessment are separate immutable events.

patch_id: 6d2c7401f8ae02a84de4c2f439acccde860c1bff
prior_verification_digest: sha256:2f00f36cee69e92d6fd01fea2172a0c8cea86858447ee1a33e658b5a18fe37ee
wsl_ci_readback_digest: sha256:d64ec2e18caf7d761033f5fbf61a34cdec380238f9c340b1ac9603e75d4c3a82
native_composition_head: 122f5b65
native_failure_log: .worktrees/orchestrator/.herd/coordinator-resume/completion-recovery-20260909/fac708-flash-review-launch-0342.log
reviewed_at: 2026-09-10T02:20:38Z

## Tests run

- `git rev-parse HEAD` — exit 0; `6cf5dfad9deb6524414f78b2c53a1745edfa0f57`.
- `git status --porcelain` — exit 0 and empty; the pinned source remains clean.
- `HERD_REVIEW_LEDGER=/Users/kampe/Personal/Herdforge/.herd/review-ledger.jsonl herd review-ingest /Users/kampe/Personal/Herdforge/.herd/review/inbox/6cf5dfad9deb-review-fac-791-6cf5dfad9deb-reassess-ead3e063942e3837.md --dry-run --json` — exit 0; exact reassessment would admit with refused 0.
- No additional source tests, provider probes, or broad checks were run in this reassessment because the source is unchanged and the request restricted reassessment to the supplied evidence.
- Coordinator-supplied evidence, not executed by this reviewer: exact-candidate clean isolated WSL `make ci` report1884, independently rehashed raw-log digest `d64ec2e18caf7d761033f5fbf61a34cdec380238f9c340b1ac9603e75d4c3a82`; native composition/build at coordinator review bootstrap HEAD `122f5b65`; native OpenCode route resolution in 12s; secondary provider request in 4s; reviewer creation; `pkg/herdr.Send` timeout with last status `working`; and exact-tab failure cleanup.

The prior source-focused tests, race/build/vet/security/hermeticity results, mutation RED/GREEN proof, and final clean status remain in the preserved prior artifact. This reassessment does not relabel them as newly executed.
