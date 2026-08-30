sha: e30e6f016cee73d94fabc0ac8ace7b5b9c419d41
branch: fac-640-native-role-signing
task: FAC-640
reviewer: pool-01
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: e30e6f016cee73d94fabc0ac8ace7b5b9c419d41
---

Scope: cmd/herd/main.go, pkg/config/config.go, pkg/dispatch/dispatch.go, plus cmd/herd/launch_policy_test.go and pkg/dispatch/dispatch_fac121_test.go. Risk tier: R2 (launch/dispatch authority resolution feeding signed task-context receipts — no direct secrets/board-mutation path, but wrong output here changes what a receipt is allowed to do).

Bug confirmed real: `Dispatcher.taskContext` previously signed the receipt's `Role` field from the raw `lane.Role` string verbatim, never resolving it through native-role policy. For any lane declaring `standing_role_policy` (a custom human-readable standing name mapped to a canonical native role, e.g. "nft-data-engineer" -> "worker"), `OpsForRole(rawRole)` returned nil and `TaskContext.Validate()` hit its `default:` branch ("task context role %q is unknown"), so `Dispatch()` always failed for such lanes at `signReceipt` -> `Issue` -> `Validate`. The fix extracts the existing `nativeLaunchRole` logic out of cmd/herd into an exported `config.NativeLaunchRole`, and wires `Dispatcher.taskContext` to resolve through it before computing ops/session-id, while leaving `DispatchResult.Lane` on the raw lane identity (`lane.Name`) so operator-facing lane tracking is unaffected.

Verification:
- `PATH=.../mise/shims:$PATH go build ./...` — clean, no errors.
- `go vet ./...` — clean, no findings.
- `go test ./pkg/config/...` — PASS (29 tests, includes existing NativeLaunchRole-adjacent coverage).
- `go test ./pkg/dispatch/... -run 'TestDispatch_NoLaunch|TestDispatch_Launch|FAC121' -v` — PASS, including the three new tests: `TestDispatch_NoLaunch_CustomStandingRoleSignsNativeWorkerTaskContext`, `TestDispatch_NoLaunch_CustomStandingRoleWithoutNativePolicyFailsClosed`, `TestDispatch_NoLaunch_OrdinaryWorkerRoleRemainsWorker`.
- `go test ./pkg/dispatch/...` (full package) — PASS, 12.9s.
- `go test ./cmd/herd/... -run 'LaunchPolicy|StandingRole|NativeRole|WorkerConfig'` — PASS, including `TestCustomStandingRoleUsesExplicitNativeWorkerPolicy` (updated call site).
- Non-vacuity (AGENTS.md invariant #2): inside this isolated pool worktree (confirmed `git rev-parse --show-toplevel` resolves under `.herd/pool/pool-01` before touching anything), swapped `pkg/dispatch/dispatch.go` and `pkg/config/config.go` to their `HEAD~1` (parent) blobs and reran the two new positive-path tests. Both failed exactly as expected with the pre-fix symptom: `task context role "nft-data-engineer" is unknown — unknown roles carry no authority (FAC-145 fail-closed)`. Restored both files to `HEAD` afterward; `git status --porcelain` confirmed clean before writing this verdict.
- `go test ./cmd/herd/... ./pkg/config/... ./pkg/dispatch/... ./pkg/router/... ./pkg/launch/...` — all pass except `pkg/launch`, which fails independently of this candidate: `TestOrdinaryRequestCannotBypassProductionDiscovery`, `TestClaudeCommandIncidentRequiresBoundHealthPolicyBeforeEffects`, `TestValidateOptionalHookWarningIsDeduplicatedAndIdentityPreserved` all fail with `hook.timeout` — reproduced identically on `HEAD~1` (the parent commit, before this candidate, in the same sandbox), and `pkg/launch` is untouched by this diff. Pre-existing environment/timing flake, not attributable to FAC-640.

Design notes:
- Error wrapping changed slightly: `cmd/herd` callers now blanket-wrap every `config.NativeLaunchRole` error with `ErrWorkerConfigPolicy` (previously only some of `nativeLaunchRole`'s branches wrapped it). Checked: no test asserts the old unwrapped text for the nil-lane/empty-role branches, and the change makes those paths consistently satisfy `errors.Is(err, ErrWorkerConfigPolicy)` — a strict improvement, not a regression.
- No import cycle: `pkg/config` already imported `pkg/router`; `pkg/router` does not import `pkg/config`. `knownLaunchRole`/`nativeLaunchRole` were fully removed from `cmd/herd/main.go` with no dangling references.
- Confirmed by reading `pkg/dispatch/taskcontext.go` that `dispatch.OpsForRole`/`TaskContext.Validate` recognize a narrower role vocabulary (worker/reviewer/verifier/recovery/integration/coordinator) than `router.KnownRole` (which also allows forge-smith/orchestrator/scout-planner/verification-gate/review-supervisor/harvest/recovery-sentinel/assayer). A lane whose native role resolves to one of those other roles would still fail at the same `Validate()` boundary post-fix. This is pre-existing and unchanged by this diff (ordinary non-standing lanes resolve to the same raw role string before and after), and `.herd/herd.yaml` currently has zero lanes using `standing_role_policy`, so no live lane is affected either way. Flagging as an out-of-scope follow-up, not a blocker for FAC-640.

Verdict: PASS. The fix is minimal, correctly scoped to its single call site (`Dispatcher.taskContext`), preserves lane identity separately from signed authority, is covered by tests that fail without the fix and pass with it, and introduces no detected regression.
