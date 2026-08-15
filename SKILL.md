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
herd review --spawn
herd board-done FAC-123
```

The coordinator owns harvesting, pull requests, merging, and cleanup. Fleet
workers commit only in their assigned worktrees. Do not treat a claim, pane,
or `in-progress` board card as completion; require a candidate SHA, verification
receipt, independent review, integration proof on `origin/main`, and provider
readback.

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
