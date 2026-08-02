---
name: herd
description: Operate the Herdforge self-forging multi-agent software factory. Use for standing agents, pulse sweeps from Kaneo, task-packet delivery to agents, review pipeline, and worktree lane management.
---

# Herdforge Agent Skill Guide

Herdforge is a **self-forging** orchestration daemon — it claims its own Kaneo board cards,
spawns/uses standing agents in herdr, and moves work through review without human intervention.

## Prerequisites

- **herdr** must be installed (`brew install herdr` or `npm install -g herdr`)
- **kaneo** CLI must be configured (`~/.config/kaneo/config.json`) when `use_cli: true`
- Workspace `wF` must exist in herdr for standing lanes

## Core Workflow

### 1. Launch standing agents (once per session)
```bash
herd standing
```
Creates three named agents in herdr workspace `wF`:
- `forge-forge-smith` (planner)
- `forge-worker` (builder)
- `forge-reviewer` (auditor)

### 2. Pulse sweep — claim and dispatch
```bash
herd pulse --role worker --spawn
```
or for forge-smith or reviewer:
```bash
herd pulse --role forge-smith --spawn
herd pulse --role reviewer --spawn
```

What happens:
1. Lists all `to-do` cards from Kaneo
2. Sorts by priority DESC, ref ASC
3. Claims the top card (moves to `in-progress` via `kaneo task status`)
4. Resolves the standing agent by name (`forge-worker`, `forge-forge-smith`, etc.)
5. Delivers a structured task packet via `herdr agent prompt`

### 3. Inspect agents
```bash
herdr agent list
herdr agent read forge-worker --source recent --lines 20
herdr agent read forge-reviewer --source recent --lines 20
```

## Commands

| Command | Description |
|---------|-------------|
| `herd init` | Scaffold `.herd/herd.yaml` and lane prompts |
| `herd preflight` | Check repo-relative path violations |
| `herd status` | Show project, provider, lane config |
| `herd standing` | Launch all lane agents in herdr tabs |
| `herd pulse --role <r> --spawn` | Claim next Kaneo card, deliver task packet to standing agent |
| `herd up <lane-name>` | Start a single lane agent |
| `herd selftest` | Run self-test suite |

## Example: Pulse with agent spawning

```bash
# 1. Launch lanes
herd standing

# 2. Run pulse — claims the next worker-eligible card
herd pulse --role worker --spawn

# Output:
#   Pulse sweep claimed task [FAC-33]: Port herd-next to Go
#   Using standing agent 'forge-worker' (tab wF:t7)
#   -> delivered task packet to forge-worker

# 3. Check worker progress
herdr agent read forge-worker --source recent --lines 30
```

## Config

The `.herd/herd.yaml` defines lanes, provider, and Kaneo integration:

```yaml
task_provider:
  type: "kaneo"
  project_id: "b939c5jzixruza3vvywrg1hs"
  use_cli: true

lanes:
  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/worker"
```

## Worktree Invariants
- Always run `herd preflight` before staging or committing
- Worktrees are `.gitignore`d — they are local-only
- Never write hardcoded absolute paths
