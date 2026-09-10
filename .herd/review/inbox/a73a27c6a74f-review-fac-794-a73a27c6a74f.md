sha: a73a27c6a74f70f36b27e252589d78751bc8b780
branch: recovery/fac-794-source-retirement
task: FAC-794
reviewer: review-fac-794-a73a27c6a74f
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 78bc5b0c0e29a8c6ff16df33bf0ee058e42fdf0e
reviewed-head: a73a27c6a74f70f36b27e252589d78751bc8b780
---
## Findings and risk

Reviewed the exact range `78bc5b0c0e29a8c6ff16df33bf0ee058e42fdf0e..a73a27c6a74f70f36b27e252589d78751bc8b780` at the pinned candidate HEAD. `herd review-classify` gives an R3 floor with deterministic verification, different-family review, security-capable review, and explicit high-risk gates. I inspected the retained prior FAIL artifact `321f3f5d0c4144fab1207a84bbdf70a590111f0850ec44856a6238e1a8083075`; the eight original findings are addressed at their prior call sites, but the current candidate still has the following independent blockers/high risks.

1. **[Blocker] The durable handoff parser does not require or consistently bind the full handoff identity, and accepts contradictory fields.** `pkg/herdr/source_retirement.go:497-568` only requires a recognized ready status and one valid candidate SHA. Task and agent are optional, and the first value wins for duplicate task, agent, candidate, branch, or status fields. Thus a report containing `Status: READY` and the exact candidate but no task/agent, or a report containing `Status: READY` followed by `Status: BLOCKED`, can be accepted. `EnrollReadySourceManifests` at `:633-638` only rejects a task when one is present, while `NativeSourceRetirementOp.observeHandoff` at `pkg/herdr/native_source_retirement.go:157-203` initializes omitted task/agent values from the manifest before evaluation. A quoted/spoofed or internally contradictory report can therefore satisfy the supposed exact task/agent READY handoff and authorize terminal closure.

2. **[Blocker] Launch provenance is still treated as an optional partial match rather than an authenticated exact source launch.** `pkg/herdr/source_retirement.go:203-224` requires only `Accepted`, task ref, and agent name; candidate SHA, worktree, branch, session, role, and repository are checked only when non-empty or are not checked at all. `findLaunchReceipt` in `pkg/herdr/native_source_retirement.go:114-154` matches task/name/worktree and accepts empty candidate/branch/session fields, does not bind repository, role, tab, pane, or terminal incarnation, and chooses a latest matching row rather than rejecting ambiguity. Enrollment at `pkg/herdr/source_retirement.go:602-607` excludes only lowercase `reviewer`/`review` and accepts arbitrary or missing roles, without checking the receipt repository against the operation repository. The actual acceptance contract has these fields, but the consumer does not fail closed when a malformed, stale, foreign, or pre-candidate accepted row is present; the current positive fixtures themselves omit `HerdrSession` in accepted receipts and still pass. This violates the packet's exact candidate/worktree/session/role/foreign-lane gate.

3. **[High] A process can start after preflight and be killed instead of blocking retirement.** `NativeSourceRetirementOp.Revalidate` checks `ActiveDescendants` at `pkg/herdr/native_source_retirement.go:382-410`, but `CloseSettledSourceTab` performs only an agent identity/status/focus re-read at `pkg/herdr/live_harness_proof.go:207-227`. It then captures `GetHostedPaneIdentity` and unconditionally calls `ReapHostedPaneSnapshot` after close (`:228-239`); it never inspects `snapshot.Foreground` or rejects newly observed foreground/descendant work. A `go test`, compiler, shell command, or other verification/editing child that appears between Revalidate and this close path is included in the captured tree and terminated, contrary to the immediate process/foreground race gate. The existing active-descendant test covers only the earlier observation and cannot prove this race-safe close behavior.

4. **[High] One blocked or observation-failed lane prevents every other eligible lane from making progress.** `RetireSourceLanesContext` returns before mutation whenever any candidate is blocked (`pkg/herdr/source_retirement.go:443-448`), and returns before mutation for any observation failure (`:438-441`). Consequently one standing/focused/foreign/ambiguous lane or one transient unrelated read failure stops all eligible settled source lanes in the same sweep, despite the acceptance requirement for bounded progress without one unrelated failure stopping eligible cleanup. The current test explicitly codifies the all-or-nothing behavior (“One lane blocked, preflight blocks everything”) rather than the required mixed-result behavior. The drain adapter further converts any blocked result into an error at `cmd/herd/drainadapt.go:190-195`.

Residual risk: the positive native and CLI fixtures preserve worktrees and branches, and the prior eight findings are not being repeated as unresolved. However, the four findings above leave authorization ambiguity and a close-time process race in an R3 destructive lifecycle path. No live cleanup, foreign resource action, board/provider write, commit, push, merge, or broad CI was performed.

patch_id: 1fa8ab442be4fbe3a2a6b3db178f858a89dc6432
verification_digest: sha256:7588563b61d986d6cc94eae912dfb6c9fb83d4c531b813f77f80504918d87f89
full_range: 78bc5b0c0e29a8c6ff16df33bf0ee058e42fdf0e..a73a27c6a74f70f36b27e252589d78751bc8b780

## Tests run

- `code-review-graph status --repo . --json` — exit 0; 452 nodes, 7,461 edges, 11 Go files, indexed at candidate SHA `a73a27c6a74f70f36b27e252589d78751bc8b780`.
- `code-review-graph detect-changes --repo . --base 78bc5b0c0e29a8c6ff16df33bf0ee058e42fdf0e --brief` — exit 0; 9 changed files, overall risk 0.60, and untested lifecycle symbols including `retireSourceLanes`, `runCleanup`, `drainActionHooks`, and `defaultDrainActionHooks`.
- `herd review-classify recovery/fac-794-source-retirement --pin a73a27c6a74f70f36b27e252589d78751bc8b780 --json` — exit 0; effective tier R3.
- `go test -count=1 -v ./pkg/herdr -run 'Test(Source|NativeSource|RetireSource|EvaluateSource|ParseStructured|Enroll)'` — exit 0; PASS.
- `go test -count=1 -v ./cmd/herd -run 'Test(Source|Drain)'` — exit 0; PASS.
- `go test -count=1 -race ./pkg/herdr ./cmd/herd -run 'Test(Source|NativeSource|RetireSource|EvaluateSource|ParseStructured|Enroll|Drain)'` — exit 0; PASS.
- `go vet ./pkg/herdr ./cmd/herd` — exit 0; PASS.
- `go run ./scripts/hermeticity/` — exit 0; PASS.
- Required compiling mutation proof: removed only the production call to `EnrollReadySourceManifests` in the isolated pool worktree, then ran `go test -count=1 -v ./cmd/herd -run '^TestSourceCleanupNativeAutomaticEnrollmentFromDurableHandoff$'` — exit 1 with the raw assertion `expected 1 retired lane, got {DryRun:false Candidates:[] Retired:0 Blocked:0 Failed:0}`. Restored the exact candidate source and reran the same command — exit 0; PASS. `git diff --check` and `git status --porcelain` were clean after restoration.

reviewed_at: 2026-09-10
