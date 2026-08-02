---
name: herd
description: Operate and inspect the Herdforge repository-local multi-agent control plane.
---

# Herdforge Agent Skill Guide

Use `herd` for repository workflow policy and Herdr for agent-session mechanics. The target lifecycle is documented in `docs/architecture/TARGET-WORKFLOW.md`; current commands are still being integrated and should be operated with explicit evidence checks.

## Start safely

```bash
herd --help
herd validate-config
herd preflight
herd status
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
herd review --spawn
herd board-done FAC-123
```

Do not treat a successful claim as successful dispatch, an `in-progress` card as worker completion, or an `in-review` card as a valid verdict. The target gates require an active lease, cwd-bound task worktree, consumed delivery receipt, committed candidate SHA, deterministic verification, different-family review, serialized integration, `origin/main` proof, and provider readback.

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
