sha: 3bcefcf747a17bdfbb59dffc122a70e807fde874
branch: recovery/fac-783-completion-lookup
task: FAC-783
reviewer: review-fac-783-3bcefcf747a1
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-base: 3278e8965e3548b41938c990c9f0c11f91a2e249
reviewed-head: 3bcefcf747a17bdfbb59dffc122a70e807fde874
---

Risk tier: R3. Exact parent and candidate verified. The graph was built at the candidate revision; it reports the changed production path through `BoardDone`, `BoardDoneFenced`, `ResolveDoneTask`, and `approveOne`, with no affected-flow edges and no test coverage for the key changed functions.

Findings:

1. [Blocker] Receipt provider revision is not bound to the live task. `pkg/sync/boarddone.go:472-475` checks only that a full receipt's `ProviderRevision` is non-empty; it never compares it with the live task revision (`provider.EncodeRevision(task)`). Both `BoardDone` and `BoardDoneFenced` can therefore close a card after its board acceptance state has changed since the receipt was minted. The new success fixture demonstrates the gap: it seals `ProviderRevision: "provider-rev-1"` for a live `in-review` task whose encoded revision is `in-review|task-exact-123|`, and the close still succeeds (`pkg/sync/boarddone_lookup_test.go:39-61`). A full receipt must be refused when its revision differs from the exact task read.

2. [High] Receipt authenticity is checked after the receipt locator is used. `BoardDone` and `BoardDoneFenced` call `ResolveDoneTask` before `validateAutomaticReceipt` (`pkg/sync/boarddone.go:293-305` and `565-580`), while `ResolveDoneTask` passes the receipt's untrusted `TaskID` to `GetTask` at line 453. `approveOne` repeats the same ordering at `cmd/herd/main.go:3857-3868`. A tampered or otherwise invalid receipt can trigger an arbitrary provider task read before its digest, repository, lifecycle, and integration proof are authenticated. Authentication must precede consumption of the locator.

3. [High] Missing task identity is laundered through the legacy ref lookup. When a receipt has no `TaskID`, `ResolveDoneTask` falls through to `resolveTaskByRef` (`pkg/sync/boarddone.go:451-477`), which calls `ListTasks` (`:484-490`) before the full receipt validator rejects the missing task ID. This violates the receipt-bound no-`ListTasks` contract and can turn an invalid full receipt into a provider-wide lookup attempt. The reduced-receipt compatibility path needs an explicit, separate rule; it must not be implemented by allowing every receipt with a missing task ID to use the legacy resolver.

4. [High] Exact identity checks accept incomplete provider records. The task ref and project comparisons are conditional on the live fields being non-empty (`pkg/sync/boarddone.go:463-470`). A successful `GetTask` response containing only the requested ID can pass these checks; in particular, a reduced receipt can then proceed without the full receipt's lifecycle/acceptance fields. The exact receipt path must reject a task whose canonical ref or project identity is absent or differs from the request.

Residual risk: Focused tests and build gates are green, but the missing revision and ordering invariants remain unverified and can permit stale acceptance evidence to close a board task. No live board mutation was performed.

## Tests run
- `code-review-graph status --repo . --json` — exit 0 (candidate-indexed graph: 324 nodes, 5366 edges, 3 files)
- `code-review-graph detect-changes --repo . --base 3278e8965e3548b41938c990c9f0c11f91a2e249 --brief` — exit 0
- `git diff --check 3278e8965e3548b41938c990c9f0c11f91a2e249 3bcefcf747a17bdfbb59dffc122a70e807fde874` — exit 0
- `go test ./pkg/sync -run '^TestBoardDone_ExactReceiptTaskLookup_NoListTasks$' -count=1` — exit 0
- `go test ./pkg/sync -count=1` — exit 0
- `go test -race ./pkg/sync -run '^TestBoardDone_ExactReceiptTaskLookup_NoListTasks$' -count=1` — exit 0
- `go test ./cmd/herd -count=1` — exit 0
- `go build ./cmd/herd` — exit 0
- `go run ./scripts/hermeticity/` — exit 0

Reviewed at: 2026-09-09T15:32:27Z
