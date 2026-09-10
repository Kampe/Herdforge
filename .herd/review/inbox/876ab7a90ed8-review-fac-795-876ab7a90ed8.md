sha: 876ab7a90ed81fa71c1731b6ca68f105703f5f4b
branch: recovery/fac-795-verification-headings
task: FAC-795
reviewer: review-fac-795-876ab7a90ed8
reviewer-family: openai
builder-family: open-weight
verdict: PASS
reviewed-base: 5fdc4e07e806ed0b75180e88a0bb0f6969a53630
reviewed-head: 876ab7a90ed81fa71c1731b6ca68f105703f5f4b
---
## Findings and risk

Reviewed exact range `5fdc4e07e806ed0b75180e88a0bb0f6969a53630..876ab7a90ed81fa71c1731b6ca68f105703f5f4b`; candidate identity, base ancestry, branch, and the two-file scope match the packet. The packet dispatches this as bounded R3; `herd review-classify recovery/fac-795-verification-headings --pin 876ab7a90ed81fa71c1731b6ca68f105703f5f4b --json` independently returned effective R2 because the change includes production parser code and tests. Required classifier gates are deterministic verification, different-family review, and integration rerun. The recorded builder family is `open-weight`, while this independent reviewer is `openai`, satisfying the family boundary.

Acceptance is met: `VerificationEvidence` tracks Markdown heading depth, includes child subsections, closes at sibling/ancestor headings, ignores heading-like lines in fenced blocks, normalizes whitespace, and returns empty for empty verification subsections. Existing inline labels and exact parser identity/provenance fields remain intact. The native seams use `Artifact.VerificationDigest()` for both ordinary ingest (`cmd/herd/reviewingest.go:323`) and host ingest (`cmd/herd/review_host_ingest.go:248`), while completion reconciliation re-parses and compares the same digest (`cmd/herd/review_complete_record.go:70`); no receipt or candidate identity bypass was introduced. The public parser tests cover nested headings, closure, fenced data, empty subsections, inline labels, prose rejection, and digest changes.

Finding 1 — Nit, non-blocking: `git diff --check` reports one extra blank line at EOF in `pkg/reviewingest/verification_test.go:266`. This is formatting-only and does not affect parsing, digest stability, or admission safety.

No blocker, high, or medium functional finding remains. Residual risk is limited to the packet’s retained builder RED/GREEN mutation evidence and the coordinator-owned integration/full-WSL-CI boundary; the reviewer did not mutate source or claim those broader checks. The packet’s retained evidence records raw RED1 against the old parser and restored GREEN0 at the candidate, with no historical artifact or ledger rewrite.

patch_id: e1faa23ae1bb07cb8af72631faec9e4748043f4f
verification_digest: sha256:e3ab03f872e036528f5cb15ad09e816113390cae67b7192ec3459acc710500d4
full_range: 5fdc4e07e806ed0b75180e88a0bb0f6969a53630..876ab7a90ed81fa71c1731b6ca68f105703f5f4b

## Tests run

Reviewer-executed:

- `go test -count=1 -run 'TestVerificationDigest|TestRepositoryVerdictTemplateIsIngestible' ./pkg/reviewingest` — PASS.
- `go test -count=1 ./pkg/reviewingest` — PASS.
- `go test -race -count=1 ./pkg/reviewingest` — PASS.
- `go vet ./pkg/reviewingest` — PASS.
- `go run ./scripts/hermeticity/` — PASS; no hermeticity violations reported.
- `git diff --check 5fdc4e07e806ed0b75180e88a0bb0f6969a53630..876ab7a90ed81fa71c1731b6ca68f105703f5f4b` — exit 2 solely for the non-blocking EOF blank-line nit above.

Retained builder evidence, not represented as reviewer execution: `make preflight` exit 1 due expected Mac `origin/main` drift; focused `go test -v -run "TestVerificationDigest" ./pkg/reviewingest` exit 1 against the unfixed parser (raw RED1); `go test -v ./pkg/reviewingest` exit 0; uncached `go test -count=1 ./pkg/reviewingest` exit 0; and `make lint all` exit 1 after its checks passed but preflight stopped on the same environmental origin/main drift. The packet reports no historical artifact, ledger, board, provider, push, merge, or install mutation.
