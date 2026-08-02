# Herdforge Second-Pass Audit — 2026-08-02

## Verdict

The first audit's conclusion still holds: Herdforge has many useful primitives, but it is not yet a fail-closed autonomous software factory. The second pass found a more specific failure pattern: the board can declare a capability complete when a commit merely names its ticket, even when the acceptance criteria are not connected to the running path.

This is not a request for more builder agents. The roster defined in the target workflow is sufficient. The immediate need is to make completion evidence, runtime wiring, and enforcement real.

## Snapshot and method

This pass first rebased onto `origin/main` at `4e6e85b`, after FAC-128 and FAC-129 were merged while the audit was running, then rebased again through `8a7543a` after the CI and scoped-review changes landed. The final audit-branch graph indexed 187 files, 1,775 nodes, and 17,875 edges. Graph results were checked against source because the graph still reports no affected cross-file flows for changes that plainly cross packages.

The board snapshot contained 128 cards: 92 done, 35 To Do, and 1 in progress. Every To Do card was unlabeled, 9 had no description, and 5 had no priority. Active Doing/In Review work was observed but not interrupted.

`make ci` passed. GitHub CI passed for `4e6e85b`, `4c9b132`, and `8a7543a`. PR #40 had no review submissions and the repository's `main` branch has no GitHub branch protection. A plain `make test-coverage` failed locally because fixture commits inherited the machine's 1Password signing configuration; the CI-equivalent hermetic Git environment passed with 49.3% total statement coverage. It reports `cmd/herd` and `pkg/lifecycle` at 0%, so the green suite does not exercise the highest-risk production wiring.

## Newly confirmed gaps

### 1. Board completion proves ticket mention, not task acceptance

`pkg/sync.MergeEvidence` accepts either any commit on `origin/main` whose subject names the ticket or any operator-supplied ancestor commit. Neither form proves that the commit is the claimed candidate, implements the card, passed verification, received an independent verdict, or satisfied acceptance criteria.

This weak oracle was used to close FAC-107, FAC-108, FAC-111, FAC-114, FAC-116, FAC-128, and FAC-129. Their comments cite only a main commit naming the ref. Several acceptance items remain objectively unmet. A commit subject must never be the authority that closes its own card.

Board `done` needs a task-bound integration receipt containing the candidate and merge SHAs, acceptance/verification digest, review verdict, author/reviewer families, lease generation, and provider readback. A manual override must be explicit, audited, and policy-limited; an arbitrary ancestor SHA is not task evidence.

### 2. `herd forge --loop` is not the callback-driven forge described by FAC-128

The new loop is a real production caller of `ForgeStep`, but its driver does not satisfy the card's outcome:

- `Signals` pane-polls `herdr agent list`; it never calls `mail.DrainCallbacks`.
- Dispatch still calls `Engine.SelectNextTask`; `ScoutQueue` and its dependency inputs are not consulted.
- `Review` invokes `herd review <ref> --spawn`. Go's flag parser stops at `<ref>`, so `--spawn` is not parsed and no reviewer is launched.
- `Approve` calls `herd approve`; it does not harvest or merge. Because in-review work has absolute precedence, one unmerged card can be retried forever while builder capacity idles.
- Herdr listing failure is converted to `Busy=0`, which opens dispatch capacity under unknown state.
- An orphaned in-progress card with no matching pane can look like an empty board and terminate the loop.
- Dispatch, review, approve, and re-nudge errors are logged and swallowed. A bounded run can return success after every action failed.
- The real driver has no tests. The loop tests use a fake driver and cannot expose CLI flag ordering, cwd, provider, Git, or Herdr failures.

FAC-138 now tracks this residual implementation explicitly. It should remain open until a hermetic end-to-end test drives a card through an owned worktree, exact-SHA verification, independent review, serialized integration, board readback, and cleanup, including crash/retry cases.

### 3. Tool execution is still an optional observation, not a dispatch gate

FAC-129 requires dispatch to fail over from a model that cannot execute tools. The merged change adds an optional `doctor-models --tool-probe` flag only. `dispatch` still uses quota-oriented `ResolveHealthyModel` and does not call the artifact probe.

The Go probe also supports only `opencode run`. It lacks the provider-specific recipes, versioned cache, TTL, and distinct UNKNOWN state present in Chainseer's tool probe. A missing recipe or harness incompatibility must not become fabricated evidence that a model is incapable, and a quota-healthy surface must not become write-capable without a current artifact receipt.

### 4. The generated completion contract is internally impossible

The tight task packet tells an agent to run `herd verify` before committing. `herd verify` requires a real commit ahead of `origin/main`, so following the packet in order must fail. The re-nudge message repeats the same verify-before-commit order.

The packet also emits absolute worktree paths despite the repository's repo-relative generated-artifact invariant, hardcodes Go build/vet/test commands, and assumes the agent can query Kaneo directly. That is not portable to arbitrary repositories, providers, or least-privilege worker credentials.

Completion should be: implement, run configured pre-commit checks, commit, identify the immutable candidate SHA, run configured verification against that SHA, then emit a structured callback. Verification commands must come from validated repository configuration rather than Go-specific literals.

### 5. CI verifies compilation, not the autonomous contract

PR #40 passed the unit workflow but had no different-family review and no production-driver integration test. The test suite therefore certified isolated control flow while missing every runtime defect above. GitHub branch protection does not require the CI check or a review, so the repository cannot enforce its own R1-R3 review invariant.

The acceptance harness must use a temporary Git remote, fake provider, fake Herdr process API, deterministic model probe, durable event store, and injected crash points. It should prove the complete lifecycle, double-runner exclusion, redelivery idempotency, stale-SHA rejection, root-cwd refusal, review-family separation, and preservation of unique work. Coverage and race targets should run under the same hermetic Git environment as `make ci` and enforce floors on core production packages.

### 6. Provider text is an untrusted control input

Herdforge is intended to harvest work from Kaneo, GitHub, Jira, and other repositories. Card titles, descriptions, comments, and linked content can therefore contain prompt injection or commands written by an untrusted reporter. Current paths place descriptions directly into privileged agent prompts or instruct agents to fetch the full card themselves.

The execution contract needs a trust boundary: structured task fields, least-privilege credentials, per-role filesystem/network/tool policy, secret denial, repository allowlists, auditable external-content provenance, and explicit handling of instructions embedded in issue text. Prompt wording alone is not a security boundary.

### 7. The live roster is still configuration-incomplete

The repository contains prompt contracts for orchestrator, scout-planner, verification, review supervision, integration, and recovery, but `.herd/herd.yaml` and `herd init --full` still register only smith, worker, and reviewer on the same OpenCode/DeepSeek family. Spawn paths do not set the task worktree as the actual Herdr cwd. During this pass, active Herdforge sessions still reported the shared checkout as their cwd, and multiple finished task tabs remained idle.

No additional prompt persona is required. The missing work is declarative registration and technical enforcement of the roles already documented. Blind pings would not repair these state and ownership defects, so no agent was pinged.

### 8. Attempted remediation reproduced the dispatch defects

After the audit, an orchestrator used the current production dispatch path to claim FAC-119 through FAC-122. The board comments claimed task worktrees and generated `task/...` branch names, but Herdr reported all four worker process cwd values as the shared repository root. Git reported the actual branches as `herd/fac-119` through `herd/fac-122`. The generated `TASK-PACKET.md` files still required `herd verify` before a commit existed.

The unsafe sessions were stopped before source edits. Each worker was relaunched through Herdr's native tab and agent API only after process inspection proved that cwd equaled its exact task worktree. Corrective board comments record the real agent, author family, worktree, and branch.

The same dispatch wave also claimed cards despite existing blocking relations: FAC-124 blocks FAC-119, FAC-120 blocks FAC-121, and FAC-121 blocks FAC-122. The package-local slices can be explored concurrently under explicit scopes, but this proves the current claim path does not enforce dependency eligibility.

Finally, a full-suite run in the FAC-120 worktree created a nested `pkg/dispatch/.herd/worktrees/fac-1` repository artifact. That confirms the test environment is not hermetic even when the suite exits successfully; FAC-135 must make test state temporary and self-cleaning.

## Kaneo actions from this pass

- Added FAC-132 for task-bound acceptance receipts.
- Added FAC-133 for least-privilege prompt-injection containment.
- Added FAC-134 for repository-agnostic task packets and verification profiles.
- Added FAC-135 for a hermetic compiled-binary factory conformance gate.
- Added FAC-136 for durable health, queue-pressure, and transition SLOs.
- Expanded FAC-138 and FAC-139 into dispatchable residual cards for the production loop and mandatory artifact probes.
- Related and archived FAC-137 as an exact duplicate of FAC-132.
- Verified `4c9b132` and its GitHub Actions run, then closed stale To Do card FAC-130 with implementation evidence.
- Rewrote 16 thin or empty descriptions, assigned impact-based priorities, and attached exactly one `worker` or `forge-smith` role to every To Do card.
- Added blocking relations from lifecycle, claim, dispatch, verification, provider, mailbox, roster, security, receipt, capability, and conformance prerequisites into FAC-138.

Post-grooming board state was 139 cards: 94 done, 35 To Do, 8 in progress, and 2 archived. All 35 To Do cards had a non-empty description, an explicit priority, and exactly one dispatch role.

## Critical reliability wave

The first implementation wave was started under `.herd/prompts/critical-wave-coordinator.md`:

| Card | Initial exclusive scope | Author family | Verified worktree branch |
|---|---|---|---|
| FAC-119 | lifecycle, outbox, canonical persistence schema | Anthropic | `herd/fac-119` |
| FAC-120 | fenced lease behavior in `pkg/claim` | Anthropic | `herd/fac-120` |
| FAC-121 | dispatch, worktree, and Herdr cwd enforcement | xAI | `herd/fac-121` |
| FAC-122 | exact-SHA receipts and mutation execution in `pkg/verifier` | OpenAI | `herd/fac-122` |

Shared CLI wiring, provider interfaces, schema reconciliation, review admission, and integration remain serialized. Every candidate is R3 and requires exact-SHA review by a different model family before merge.

## Corrected implementation order

1. Build the durable lifecycle/outbox and fenced claim service.
2. Fail-close provider adapters and make dispatch atomic, immutable-base, repo-namespaced, and cwd-enforced.
3. Make verification repository-configured, exact-SHA, non-vacuous, and hermetically tested.
4. Replace commit-message completion with task-bound acceptance and integration receipts.
5. Enforce different-family review and serialized integration before board completion.
6. Register the existing complete fleet roster and enforce versioned artifact capability probes at every write-capable launch.
7. Add least-privilege handling for untrusted provider content.
8. Add the end-to-end crash/concurrency conformance harness and protected-branch policy.
9. Rebuild the forge loop on durable callbacks plus reconciliation; fail closed on every unknown dependency.
10. Add durable health/SLO reporting, then tune capacity for throughput.

Until these are demonstrated, `herd forge --loop` should be treated as experimental assisted automation, not an unattended authority.
