---
name: herd
description: Operate the Herdforge self-forging software factory with Kaneo, Herdr, isolated worktrees, verification, review, and cleanup.
---

# Herdforge Agent Skill Guide

Fleet routing and persistence are governed by `.herd/prompts/routing.md`; packets must reference that file instead of hardcoding mutable targets.

Herdforge is a Go control plane for a self-forging fleet. The checked-in
configuration uses Kaneo as its task provider and Herdr as the execution plane.
Other providers, including Linear, remain opt-in when a repository operator
selects and configures them explicitly.

## Prerequisites

- `herdr` is installed and authenticated.
- `kaneo` is authenticated for the checked-in project (or the selected provider
  is configured in a local `.herd/herd.yaml.local`).
- The repository has a valid `.herd/herd.yaml`.

## Start safely

```bash
herd --help
herd validate-config
herd preflight
herd status
herd forge --loop
```

## Fleet and repository inspection

```bash
herd next
herd attention
herd resources
herd throughput
herd worktrees
herd overlap
herd unmerged --all
herd board-sync
```

Use `herd <command> --help` at the checked-out revision before mutating state.

## Dispatch and review

```bash
herd pulse --role worker --spawn
herd dispatch FAC-123
herd review FAC-123
herd review --pool FAC-123
herd review-ingest <verdict-file>
herd harvest-merge FAC-123
herd approve FAC-123
herd board-done FAC-123
```

The `herd review --pool <ref>` path does not perform a Kaneo provenance lookup.
It accepts either an existing ticket ref or a real Git branch name. A bare
ticket-style ref (for example `FAC-123`) is resolved case-insensitively to the
candidate directory `.herd/worktrees/<lowercase-ref>`, preserving the normal
`herd dispatch <ticket>` flow. If that ticket worktree is not present, a real
branch name—including names with `/`—is resolved through Git's checked-out
worktree metadata and branch verification. Branch names are looked up as Git
data, not appended to a filesystem path, so a slash is not treated as path
traversal. The command still requires the resolved candidate worktree and
candidate commit to exist.

For un-ticketed work discovered outside the normal dispatch flow, no ticket is
required: run the pool review with the branch name already checked out in its
worktree. If you want the conventional ticket workflow instead, mint or assign
the ticket and make the existing branch available at the required path before
starting review. For example, from the repository root:

```bash
git worktree add .herd/worktrees/fac-123 <existing-branch>
herd review --pool FAC-123
```

If `<existing-branch>` is already checked out in another worktree, move or
rename that ad hoc worktree first (or otherwise release the branch), then run
`git worktree add` with the required `.herd/worktrees/<lowercase-ref>` path.
Alternatively, pass the existing branch name directly, such as
`herd review --pool goal/review`. The pool review command does not mint a
ticket, locate an arbitrary unregistered path, or validate a candidate SHA
against a task card.

### Durable goal guard

`herd goal-guard` protects a lane from stopping before its durable goal is
met. It stores the goal in the current worktree at `.herd/goal-guard.json` by
default, so each lane has isolated state. Select exactly one mode:

```bash
herd goal-guard --set --lane <lane> --task <task> --owner <owner> --generation <n> [--max <n>] [--expires <RFC3339>]
herd goal-guard --check < evidence.json
herd goal-guard --stop-hook
herd goal-guard --clear
```

`--set` creates or replaces the durable goal; `--check` evaluates evidence
JSON (and also accepts a Claude Stop hook payload); `--stop-hook` evaluates the
goal using Claude's Stop hook contract; and `--clear` removes the goal.
`--max 0` (the default) allows continuation until a stop condition is met;
positive values bound the number of continuations.

The Stop hook exits quietly with no decision when no goal exists, and returns
`{"decision":"block"}` while an active goal is not met. It stops blocking
once the goal is completed, the lease is lost, the lane is held or winding
down, the goal expires, or its continuation limit is reached. When the input
contains `stop_hook_active`, the previous block has already been delivered for
that stop attempt, so the hook exits successfully to avoid a re-block loop.
After completion, clear the durable state with `herd goal-guard --clear`.

Goal guards are not installed for ordinary `herd dispatch` workers. The
current `herd standing` raise path makes a best-effort `--set` call for raised
standing lanes; an installation failure leaves the lane running with the
Stop hook's quiet no-goal behavior. Wiring guards into other launch paths is a
separate design decision.

The review-harvest supervisor owns candidate admission, reviewer dispatch and
retry, verdict ingest, author feedback, and ephemeral reviewer cleanup. It
sends only an exact PASS plus merge-ready handoff to the coordinator. The
coordinator owns harvesting, merging, board approval, and final generation-
fenced cleanup of implementation panes and one-off worktrees; standing lanes
are never auto-closed. Do not wait on fleet feedback before dispatching review.
Do not treat a claim, pane, or `in-progress` board card as completion; require
a candidate SHA, verification receipt, independent review, integration proof
on `origin/main`, and provider readback.

The supervisor queue is durable per exact candidate SHA. `admitted`, `launched`,
`verdict-retained`, `author-notified`, `harvest-ready`, and `cleanup-candidate`
are distinct lifecycle states; pulse exposes exact refs for pending and
needs-review entries. A PASS on one SHA never silently supersedes a FAIL on
another; record an explicit refutation or supersession event.

## Herdr is the execution plane

Use Herdr for all agent lifecycle operations. Use `herdr agent list/read/prompt`
for state and delivery, and `herdr tab close` only for an exact finished
ephemeral tab. Do not use repository-specific `bin/herd-*` shell orchestration
scripts as a substitute for Herdforge. Herdforge dispatch selects the harness
and model through routing; supported harnesses are Codex, Claude, Grok, AGY,
and OpenCode. Pi is not a required or default lane.

Before and during a wave, run `herd quota-supervisor --read-only` (or
`herd quota --json`) and `herd pulse --json`. Quota errors must be read from
recent pane evidence, not inferred from an agent's `working` state. Reroute a
surface only after recording the provider error and preserving the standing
lane.

The writable quota supervisor persists `.herd/quota-wake.json` when a future
Claude five-hour reset is observed. Its stable key deduplicates the platform
cron/launchd wake; re-read live quota before dispatch after that wake.

For review handoffs, target the configured `review-supervisor` lane. Reviewers
report PASS/FAIL and numbered findings to that supervisor, never directly to
the coordinator. On FAIL, the supervisor returns findings to the author and
retries the same candidate lifecycle; on PASS, it tells the coordinator to
merge. The coordinator closes implementation/review panes only after merge.

## Providers and harnesses

The checked-in `.herd/herd.yaml` selects `kaneo` explicitly. A local profile may
select another wired provider, such as `linear`, when its credentials and
project/team settings are supplied. Provider selection is explicit and never
auto-detected.

Harness/model selection is runtime routing policy. Supported vendor harnesses
include Codex, Claude, Grok, AGY, and OpenCode; do not add or rely on Pi lanes.
The author and reviewer must use different model families for R1–R3 work.

## Worktree invariants

- Work only in the assigned task worktree; keep the shared checkout for
  coordination and integration.
- Run `herd preflight` before staging or committing.
- Keep tracked configuration and generated artifacts repository-relative.
- Clean exact task worktrees and dead panes after completion, while preserving
  any unique dirty work for recovery.

## Local skill placement

This guide is the repository product skill. `.herd/skills/herd.md` mirrors the
same contract for repository-local discovery. The generic Herdr operations
skill is installed at `~/.claude/skills/herdr`; it covers Herdr mechanics, not
Herdforge's review, receipt, board, or cleanup policy.
