sha: e292e69cd1dfaabbb5d5571a007e3af527508bac
branch: recovery/fac-784-native-list
task: FAC-784
reviewer: review-fac-784-e292e69cd1df
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-base: 34302e89fe6f70157d9550b84f9e36ac41d1749c
reviewed-head: e292e69cd1dfaabbb5d5571a007e3af527508bac
---

Risk tier: R3. The exact base-to-head diff was reviewed at the pinned candidate SHA. The graph index was current at the candidate (93 nodes, 1,582 edges); the changed production path is `pkg/provider.KaneoProvider.ListTasks`, consumed by board selection, dependency, sync, and review flows.

Findings:

1. [High] The native board envelope's project identity is not validated (`pkg/provider/kaneo.go:662-669`, `pkg/provider/kaneo.go:851-859`). The deployed response identifies the board in `data.id`, but the decoder does not model or check that field. Task-level `projectId` is checked only when present, so a valid-looking response from another board—or a response whose tasks omit `projectId`—is accepted for the requested project. This can feed cross-project or wrong-board tasks into authority-bearing callers. The response must reject a missing or mismatched top-level project identity before returning any tasks.

2. [High] Pagination metadata is decoded but not validated (`pkg/provider/kaneo.go:670-675`, `pkg/provider/kaneo.go:871-880`). `page`, `pageSize`, and `total` are ignored, and `totalPages` is treated as an optional early-stop hint. A response such as page 1 with one task, `total: 100`, and `totalPages: 1` returns success with an incomplete snapshot; missing/zero metadata can also be accepted when an empty page is returned. This violates the fail-closed complete-pagination requirement and allows a malformed or prematurely truncated board read to look authoritative. Validate the required metadata against the requested page and accumulated task count, and reject contradictions or missing pagination instead of returning partial success.

No other finding was established in the exact diff: the route/query shape, HTTP 200 error rejection, task identity checks present in the implementation, duplicate-page refusal, later-page error refusal, status filtering, and candidate restoration were exercised as described below.

## Tests run
- `go test ./pkg/provider -run '^TestKaneoProvider_ListTasks$' -count=1` against the parent route implementation: exit 1 (expected RED; the handler received `/api/task` and returned 404).
- Restored `pkg/provider/kaneo.go` to candidate SHA `e292e69cd1dfaabbb5d5571a007e3af527508bac`; focused ListTasks and bulk-label tests: exit 0.
- `go test ./pkg/provider -count=1`: exit 0.
- `go test -race ./pkg/provider -count=1`: exit 0.
- `go build ./cmd/herd`: exit 0.
- `go run ./scripts/hermeticity/`: exit 0.
- `git diff --check 34302e89fe6f70157d9550b84f9e36ac41d1749c e292e69cd1dfaabbb5d5571a007e3af527508bac`: exit 0.
- Final `git status --porcelain`: clean.

Residual risk: FAIL. Until the envelope identity and pagination invariants are enforced, native HTTP ListTasks can return an apparently successful wrong-project or incomplete snapshot.

reviewed_at: 2026-09-09T10:13:53-05:00
