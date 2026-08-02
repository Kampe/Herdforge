# Herdforge

**Herdforge** is a self-forging multi-agent orchestration daemon — it writes its own code, claims its own Kanban cards, spawns AI agents to work them, and moves them through review. Written in Go.

[![CI Workflow](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

---

## The Loop

Herdforge runs a self-reinforcing cycle using three standing agent lanes in [herdr](https://github.com/mariozechner/herdr) terminal workspaces:

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ forge-smith │────▶│   worker    │────▶│  reviewer   │
│ (planner)   │     │ (builder)   │     │ (auditor)   │
└─────────────┘     └─────────────┘     └─────────────┘
       │                    │                    │
       └──────────┬─────────┴─────────┬──────────┘
                  ▼                   ▼
           Kaneo Kanban         GitHub repo
        (cards flow: to-do →   (worktrees →
         in-progress → review →  commits → push)
         done)
```

1. **`herd standing`** — Spawns all three agents as named opencode sessions in herdr (`forge-forge-smith`, `forge-worker`, `forge-reviewer`)
2. **`herd pulse --role <role> --spawn`** — Polls the Kaneo board, claims the highest-priority `to-do` matching the role, and delivers a structured task packet to the standing agent
3. **Worker agent** — Receives ref/title/description/worktree path, implements the change, commits
4. **`herd review --spawn`** — Finds `in-progress` cards, dispatches a review packet to `forge-reviewer`, moves card to `review` status
5. **`herd approve`** — Finds `review` status cards and moves them to `done`
6. **`herd forge`** — Runs the full cycle in one shot: pulse + review + approve

Everything runs on `deepseek-v4-flash` via opencode — cheap enough to keep all three agents running continuously.

### Self-Replication

The forge can clone itself:

```bash
herd clone https://github.com/Kampe/Herdforge.git
cd Herdforge
herd standing    # launches all 3 agents
herd forge       # runs the full pulse → review → approve cycle
```

After cloning, link to your Kaneo project:
```bash
kaneo link -w <workspace> -p <project>
```

---

## Requirements

- **Go 1.24+**
- **Git**
- **herdr** — terminal workspace manager (`brew install herdr` or `npm install -g herdr`)
- **(Optional) Kaneo** — kanban board at `kanban.kampe.kluster` for task tracking

---

## Quickstart

```bash
# Build & test
go build ./...
go test ./...

# Clone Herdforge (self-replication)
herd clone https://github.com/Kampe/Herdforge.git
cd Herdforge

# Or seed config in an existing repo
herd init --full

# Launch all agent lanes
herd standing

# Claim & dispatch a task to a standing agent
herd pulse --role worker --spawn

# Review in-progress work
herd review --spawn

# Approve reviewed work
herd approve

# Full cycle in one shot
herd forge

# Inspect agent status
herdr agent list
herdr agent read forge-worker --source recent --lines 20
```

---

## Configuration (`.herd/herd.yaml`)

```yaml
version: "1"

project:
  name: "Herdforge"
  default_branch: "main"

task_provider:
  type: "kaneo"           # kaneo | memory
  project_id: "your-project-id"
  use_cli: true            # use kaneo CLI (no API key needed)

lanes:
  - name: "forge-smith"
    role: "forge-smith"
    agent_kind: "opencode"
    prompt: ".herd/prompts/smith.md"
    worktree: ".worktrees/smith"
    model: "deepseek-v4-flash"

  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    prompt: ".herd/prompts/worker.md"
    worktree: ".worktrees/worker"
    model: "deepseek-v4-flash"

  - name: "reviewer"
    role: "reviewer"
    agent_kind: "opencode"
    prompt: ".herd/prompts/reviewer.md"
    worktree: ".worktrees/reviewer"
    model: "deepseek-v4-flash"
```

---

## CLI Commands

| Command | Description |
|---------|-------------|
| `herd init` | Scaffold `.herd/herd.yaml` and lane prompts |
| `herd init --full` | Scaffold full 3-lane forge config (smith, worker, reviewer) |
| `herd clone <url> [dir]` | Clone a Herdforge repo and bootstrap the forge |
| `herd preflight` | Check for repo-relative path violations |
| `herd status` | Show project, provider, and lane config |
| `herd standing` | Launch all lane agents in herdr tabs |
| `herd pulse --role <r> --spawn` | Claim next `to-do` task, route to standing agent |
| `herd review --spawn` | Claim `in-progress` tasks for reviewer, move to `review` status |
| `herd approve` | Move `review`-status cards to `done` |
| `herd forge` | Full cycle: pulse + review + approve |
| `herd up <lane-name>` | Start a single lane agent |
| `herd selftest` | Run the self-test suite |

---

## How Cards Flow

1. Cards in Kaneo with `status: to-do` are candidates
2. `herd pulse` sorts by `priority DESC, ref ASC` and claims the top match
3. The card is moved to `in-progress` via the task provider
4. A structured task packet (ref, title, description, worktree path, workflow steps) is delivered to the standing agent
5. The agent works the task, commits, and signals completion
6. `herd review --spawn` picks up `in-progress` cards, dispatches a review packet to the reviewer agent, moves card to `review`
7. `herd approve` finds `review`-status cards and moves them to `done`
8. The next `herd forge` or `herd pulse` picks up the next card from `to-do`

---

## Self-Forging Architecture

Herdforge uses itself to build itself. Every package in the [ownership grid](AGENTS.md#2-complete-package-ownership-grid-30-packages) can be developed by a worker agent spawned from `herd pulse`, reviewed by a reviewer agent, and merged — all while the forge runs on cheap local inference (deepseek-v4-flash via opencode).

There are no API keys required for Kaneo when `use_cli: true` — the `kaneo` CLI handles authentication via its configured config file.

---

## License

MIT
