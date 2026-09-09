sha: f503cfe52aeda4cd3c28deed98aee5258b91f67d
branch: harvest/fac668-contract-887
task: FAC-668
reviewer: review-fac-668-f503cfe52aed
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 93d9c2ba42034d18938adcb28d584790adc0dd91
reviewed-head: f503cfe52aeda4cd3c28deed98aee5258b91f67d
---

task_ref: FAC-668
candidate_sha: f503cfe52aeda4cd3c28deed98aee5258b91f67d
patch_id: 14be073c64298ae7d2e008ed278d1cbb3be9537d
risk_tier: R3
verdict: FAIL
author_family: open-weight
reviewer_family: openai
verification_digest: sha256:64fc719c184d1ff3b1f4178b865901306d9f3a8f07a55227f1fd7edc441f6463
reviewed_at: 2026-09-09T13:22:17-05:00

#### Findings

1. [High] The exact-tree contract gate does not precede every provenance mutation required by FAC-668. In `cmd/herd/review_pool.go:134-148`, an unproven candidate supplied with `--builder-family` calls `recordAssertedBuilderLaunch`, which writes an operator-asserted launch row through `reviewledger.EnsureRecord` (`:1419-1434`). The new `verifyCandidateTreeContract(root, sha)` refusal is later at `:220-229`. Therefore a candidate whose reviewer contract is missing, a symlink, or a directory can leave operator-asserted provenance behind before the candidate is refused. This violates the packet's ordering requirement (contract validation before any provenance operation) and can make a failed candidate appear provenance-admitted on a retry. The gate must run before the asserted-family write, or the write must be transactionally removed when validation fails.

The candidate tree itself has both required paths as regular `100644 blob` entries. The defect is the ordering of the pre-gate side effect, not the `git ls-tree` mode check.

#### Scope and residual risk

The review covered the complete four-commit range `93d9c2b..f503cfe`, not only the final commit. Focused behavior, race, build, vet, and hermeticity checks passed. The package-wide test command was bounded and interrupted after roughly three minutes with no output; it is not claimed as a pass. The ordering finding remains merge-blocking for this R3 contract/ownership change.

## Tests run

- `go version` — exit 0 (`go1.26.6 darwin/arm64`).
- `code-review-graph status` — exit 0; 48 nodes, 851 edges, 5 changed files analyzed.
- `code-review-graph detect-changes --base 93d9c2ba42034d18938adcb28d584790adc0dd91 --brief` — exit 0; reported overall risk 0.55 and the affected functions.
- `go test ./cmd/herd -run 'Test(PoolReview|VerifyCandidateTreeContract|ReviewContract|VerifySurfaceCandidate|NoLaunch|ReviewPacket|PacketContract)' -count=1` — exit 0.
- `go test -race ./cmd/herd -run 'Test(PoolReview|VerifyCandidateTreeContract|ReviewContract|VerifySurfaceCandidate|NoLaunch|ReviewPacket|PacketContract)' -count=1` — exit 0.
- `go build ./...` — exit 0.
- `go vet ./...` — exit 0.
- `go run ./scripts/hermeticity/` — exit 0.
- Mutation control: temporarily bypassed `verifyCandidateTreeContract`, ran `go test ./cmd/herd -run '^TestPoolReviewRefusesUnownedContractBeforePoolMutation$' -count=1` — exit 1 as expected; the test observed pool/surface/fleet mutation. Restored `cmd/herd/review_pool.go` with `git checkout --` and verified clean status.
- `go test ./cmd/herd -count=1` — exit 1 after reviewer interruption at approximately 3m10s with no output; not treated as a passing result.
- `git diff --check 93d9c2ba42034d18938adcb28d584790adc0dd91 f503cfe52aeda4cd3c28deed98aee5258b91f67d` — exit 0; final `git status --porcelain` was clean.
