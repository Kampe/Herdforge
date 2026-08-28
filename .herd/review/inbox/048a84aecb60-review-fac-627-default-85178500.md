sha: 048a84aecb603537c06df8400bbe9811b0979074
branch: fac-627-default-pool
task: FAC-627
reviewer: w4:t81
reviewer-family: openai
builder-family: anthropic
verdict: PASS
reviewed-head: 048a84aecb603537c06df8400bbe9811b0979074
---

PASS. The branch changes `quotaState` so every non-empty, recorded pool name—including `default`—uses its pool-specific burn state, while missing and unscoped pools retain the provider aggregate fallback. Direct source inspection confirmed the behavior is consumed by availability, ranking, launch decisions, and standing-provider capacity checks.

Evidence:

- Compared candidate head `048a84aecb603537c06df8400bbe9811b0979074` with merge base `8981f941f6c2f47e3b70c1c8e76dc110fef0d1a0`: one production condition changed in `pkg/router/route.go`; five regressions were added in `pkg/router/default_pool_test.go`.
- The five focused FAC-627 tests passed, and `go test ./pkg/router -count=1`, `go vet ./pkg/router`, and branch `git diff --check` passed. The router package also passed in the repository-wide shuffled suite.
- Non-vacuity was proven in the leased pool and restored afterward. Reintroducing `pool != "default"` made the direct healthy-default test and end-to-end decision test fail with the expected aggregate-exhaustion refusal. A separate always-default mutation made all three negative guards fail: exhausted sibling, unknown pool fallback, and unscoped aggregate fallback.
- The final worktree was clean and `reviewed-head` matched the candidate.

Limitations and unrelated gate evidence:

- `make lint all` could not complete: the unchanged security gate reduces an all-empty gosec issue set to JSON `null` and then attempts `.Issues[]`, producing `Cannot iterate over null`.
- A separately tracked `make all` completed with failures in untouched packages (`pkg/harness`, `pkg/herdr`, `pkg/launch`, `pkg/review`, `pkg/sync`, and `pkg/verifier`); `pkg/router` passed. Individual reruns showed the `pkg/herdr` failure was shuffle-sensitive, while the others reproduced as environment/fixture failures unrelated to this two-file branch delta.
- Code-review-graph evidence was not available because the installed wrapper reported its pinned runtime missing. The packet-required `docs/prompts/review-contract.md` was absent from both the candidate tree and `origin/main`; review followed the packet and repository contract supplied to the lane.
