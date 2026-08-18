---
name: herd
description: Operate the Herdforge self-forging software factory with Kaneo, Herdr, isolated worktrees, verification, review, and cleanup.
---

# Herdforge Agent Skill Guide

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
herd review-ingest <verdict-file>
herd harvest-merge FAC-123
herd approve FAC-123
herd board-done FAC-123
```

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
