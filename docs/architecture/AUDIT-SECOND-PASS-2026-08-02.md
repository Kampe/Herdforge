# Herdforge Second-Pass Audit — 2026-08-02

## Verdict

The first audit's conclusion still holds: Herdforge has many useful primitives, but it is not yet a fail-closed autonomous software factory. The second pass found a more specific failure pattern: the board can declare a capability complete when a commit merely names its ticket, even when the acceptance criteria are not connected to the running path.

This is not a request for more builder agents. The roster defined in the target workflow is sufficient. The immediate need is to make completion evidence, runtime wiring, and enforcement real.

## Snapshot and method

This pass rebased onto `origin/main` at `4e6e85b`, after FAC-128 and FAC-129 were merged while the audit was running. The code-review graph was rebuilt at that revision and indexed 187 files, 1,774 nodes, and 17,862 edges. Graph results were checked against source because the graph still reports no affected cross-file flows for changes that plainly cross packages.

The board snapshot contained 128 cards: 92 done, 35 To Do, and 1 in progress. Every To Do card was unlabeled, 9 had no description, and 5 had no priority. Active Doing/In Review work was observed but not interrupted.

`make ci` passed. GitHub CI passed for `4e6e85b`. PR #40 had no review submissions and the repository's `main` branch has no GitHub branch protection. A plain `make test-coverage` failed locally because fixture commits inherited the machine's 1Password signing configuration; the CI-equivalent hermetic Git environment avoids that failure. Coverage also reports `cmd/herd` and `pkg/lifecycle` at 0%, so the green suite does not exercise the highest-risk production wiring.

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

FAC-128 should remain open until a hermetic end-to-end test drives a card through an owned worktree, exact-SHA verification, independent review, serialized integration, board readback, and cleanup, including crash/retry cases.

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

## Corrected implementation order

1. Replace commit-message completion with task-bound acceptance and integration receipts.
2. Build the durable lifecycle/outbox and fenced claim service.
3. Make dispatch atomic, immutable-base, repo-namespaced, and cwd-enforced.
4. Make verification repository-configured, exact-SHA, non-vacuous, and hermetically tested.
5. Enforce different-family review and serialized integration before board completion.
6. Rebuild the forge loop on durable callbacks plus reconciliation; fail closed on every unknown dependency.
7. Enforce versioned artifact capability probes at every write-capable launch.
8. Add the end-to-end crash/concurrency conformance harness and protected-branch policy.
9. Add least-privilege handling for untrusted provider content.
10. Register the existing complete fleet roster and then tune capacity for throughput.

Until these are demonstrated, `herd forge --loop` should be treated as experimental assisted automation, not an unattended authority.
