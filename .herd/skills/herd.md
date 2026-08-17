---
name: herd
description: Operate and inspect the Herdforge repository-local multi-agent control plane.
---

# Herdforge Agent Skill Guide

Use `.herd/prompts/routing.md` as the mutable routing and persistence contract for every lane and kick.

Use `herd` for repository workflow policy and Herdr for agent-session mechanics.
The commands below are the live contract; keep evidence checks around every
mutation.

## Start safely

```bash
herd --help
herd validate-config
herd preflight
herd status
herd forge --loop
```

For Herdforge development, the hermetic repository gate is:

```bash
make ci
```

## Read-only fleet and repository inspection

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

Prefer these before mutation. Unknown provider, Git, review, or session state must remain unknown rather than being converted to an empty/clean result.

## Task operations

Use `herd <command> --help` at the checked-out revision before a mutation. Common bounded operations include:

```bash
herd pulse --role worker --spawn
herd dispatch FAC-123
herd review FAC-123
herd review-ingest <verdict-file>
herd harvest-merge FAC-123
herd approve FAC-123
herd board-done FAC-123
```

Do not treat a successful claim as successful dispatch, an `in-progress` card as
worker completion, or an `in-review` card as a valid verdict. The gates require
an active lease, cwd-bound task worktree, consumed delivery receipt, committed
candidate SHA, deterministic verification, different-family review, serialized
integration, `origin/main` proof, and provider readback.

The standing review-harvest supervisor owns review admission, reviewer launch,
bounded retries, verdict ingest, author feedback, and ephemeral reviewer-tab
cleanup. It sends only merge-ready PASS handoffs to the coordinator. The
coordinator merges, approves, and generation-fenced-cleans implementation
panes and one-off worktrees; standing lanes remain open. Feedback census never
blocks review dispatch.

Review state is durable per exact SHA: admitted, launched, verdict-retained,
author-notified, harvest-ready, cleanup-candidate, and closed. Pulse reports
the refs behind pending and needs-review counts. Refutation and supersession
must be explicit ledger events; chronology alone never replaces a FAIL.

Use Herdr directly for lifecycle and delivery (`herdr agent list/read/prompt`,
`herdr tab close`). Do not route Herdforge work through repository `bin/herd-*`
shell scripts. Harness selection is router-driven across Codex, Claude, Grok,
AGY, and OpenCode; Pi is neither required nor a default.

Run `herd quota-supervisor --read-only` and `herd pulse --json` during a wave.
The quota supervisor reads bounded recent pane output so a lane marked
`working` but stalled on provider quota is rerouted with durable evidence.
The writable quota supervisor records `.herd/quota-wake.json` for the next
Claude five-hour reset. Treat its stable key as an idempotent scheduler job,
and re-read quota before sending work after the wake.

## Agent invariants

- Work only in the assigned task worktree; never implement in the shared checkout.
- Never remove sibling worktrees or rewrite unowned refs.
- Run the configured preflight and verification gate before reporting completion.
- Report an immutable candidate SHA and lease generation.
- Never author and review the same change; R1–R3 review must cross model families.
- Only the integration owner mutates the default branch.
- Board `done` follows `origin/main` evidence.
- Paths in tracked configuration and generated artifacts remain repository-relative.

Spawn role contracts from `.herd/prompts/`; do not replace technical enforcement with prompt instructions.
