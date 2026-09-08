sha: 3c4c3f08cdf7d1c80c86384dc863decaaa606f88
branch: repair/fac-765-authenticated-host-ingest
task: FAC-765
reviewer: pool-01
reviewer-family: openai
builder-family: unrecorded
verdict: BLOCKED
reviewed-base: 66257f366118826a3134becffbbe7d31e02f5b15
reviewed-head: 3c4c3f08cdf7d1c80c86384dc863decaaa606f88
---
## Findings and risk

Reviewed exactly the two-file diff from `66257f366118826a3134becffbbe7d31e02f5b15` to `3c4c3f08cdf7d1c80c86384dc863decaaa606f88`. The change is an independently classified R2 review-ledger workflow change: it adds host-aware retry supersession to `pkg/review.Ledger.VetoSHAs` and adds projection tests. The candidate behavior itself has no code-level finding from this review; same-host retry clearing and cross-host dissent preservation are covered and pass.

1. **[Blocker] The review packet is not admissible under the reviewer contract.** It does not provide the required patch ID, diff base, risk tier, author model family, verification digest, or acceptance criteria. Its required provenance is explicitly `builder-family: unrecorded`, so author-family independence cannot be established.
2. **[Blocker] The packet-required `docs/prompts/review-contract.md` is absent from both the candidate and its parent tree.** The mandated contract could not be read or used to validate acceptance criteria.

Because these identity and acceptance bindings cannot be proven, this is `BLOCKED`, not merge authority. Residual verification risk also remains because the repository-wide gates are not green in this host: `make lint all` reached the security gate but failed on `jq: Cannot iterate over null`, and `make all` had unrelated harness-hook, launch, host-credential, verifier-toolchain, and bin-parity failures. The changed `pkg/review` and `pkg/reviewledger` suites passed.

## Tests run

- `code-review-graph status` and change/impact queries: graph available for the candidate (62 nodes, 422 edges); changed files are `pkg/review/ledger.go` and `pkg/review/ledger_veto_projections_test.go`.
- `make preflight`: passed; reported only stale local herd provenance and the expected no-fence-broker warning.
- `go test ./pkg/review`: passed.
- `go test ./pkg/reviewledger`: passed.
- Non-vacuity proof: temporarily replaced only `pkg/review/ledger.go` with the parent blob inside the leased pool; `go test ./pkg/review -run '^TestVerdictProjectionsShareRetryAuthority$' -count=1` failed at `same_host_retry_clears_named_veto` because parent `VetoSHAs` returned the veto. Restored the candidate file with `git checkout -- pkg/review/ledger.go`; worktree is clean.
- `make lint all`: failed in the existing security gate after vet, hermeticity, and package-inventory checks passed; `jq` rejected the gitleaks report as null.
- `make all`: build, nested contract tests, hermetic compilation, and `pkg/review`/`pkg/reviewledger` passed; the full shuffled suite failed in unrelated environment-sensitive packages listed above.

## Author instructions

Resolve the missing review contract and supply an admitted packet containing the exact patch ID, base SHA, risk tier, authenticated builder family, verification digest, and acceptance criteria, then re-run review admission on this exact candidate if it remains the target.
