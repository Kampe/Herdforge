sha: c9def38becb980ecb7dd2407c63f8787c1c8023f
branch: herd/fac-657
task: FAC-657
reviewer: w4
reviewer-family: google
builder-family: openai
verdict: PASS
reviewed-head: c9def38becb980ecb7dd2407c63f8787c1c8023f
---
The candidate commit correctly implements a strict fallback behavior when evaluating quotas, failing closed as expected for quotas that are missing, stale, or marked with errors.
The changes to `cmd/herd/review_pool.go` and `pkg/router/launch.go` are sound and adequately verified by the new tests in `cmd/herd/review_routing_test.go`.
Tests for the affected packages (`cmd/herd` and `pkg/router`) pass successfully.
