sha: 0550681fc4897a345719b79e03f66b09cba93984
branch: herd/fac-631
task: FAC-631
reviewer: antigravity
reviewer-family: google
builder-family: human
verdict: FAIL
reviewed-base: 323169d6258525034f61566967e4d94aa45290b7
reviewed-head: 0550681fc4897a345719b79e03f66b09cba93984

---
1. Architecture / Tier Semantics Bug: `review-ingest` classifies against mutable `origin/main` rather than artifact `reviewed-base`. The addition of `paths, _, _, err := diffStat("origin/main", a.SHA)` uses the local tracking branch `origin/main` to determine the risk tier. If `origin/main` has advanced since the review was conducted or if the candidate was reviewed against a different base branch, this calculates an incorrect risk tier disconnected from the actual range analyzed by the reviewer. It must use the declared `a.ReadBase`.

Exact Command Exits:
- `make preflight`: Exited with code 2 due to workspace drift (`Preflight failed: main/origin/main diverged: main is 0 commit(s) ahead and origin/main is 7 commit(s) ahead`).
- `go test -count=1 ./pkg/reviewledger/ ./cmd/herd/`: The test `cmd/herd` hung indefinitely in the background and was forcefully terminated via `SIGQUIT`.
- `go test -count=1 ./cmd/herd -run ^TestReviewIngestRiskTierReachesReceiptReconcile$`: Passed (code 0).
- `go run ./scripts/hermeticity/`: Passed (code 0).
- `go build ./...`: Passed (code 0).
- `git diff --check HEAD~1..HEAD`: Passed (code 0).
