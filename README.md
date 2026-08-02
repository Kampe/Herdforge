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
2. **`herd pulse --role <role> --spawn`** — Polls the Kaneo board, claims the highest-priority task matching the role, and delivers a structured task packet to the standing agent via `herdr agent prompt`
3. **Worker agent** — Receives ref/title/description/worktree path, implements the change, commits
4. **Reviewer agent** — Reviews the diff, moves card to `done`
5. **Next pulse** — Picks up the next card from `to-do`

Everything runs on `deepseek-v4-flash` via opencode — cheap enough to keep all three agents running continuously.

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

# Seed config
herd init

# Launch all agent lanes
herd standing

# Claim & dispatch a task to a standing agent
herd pulse --role worker --spawn

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
| `herd preflight` | Check for repo-relative path violations |
| `herd status` | Show project, provider, and lane config |
| `herd standing` | Launch all lane agents in herdr tabs |
| `herd pulse --role <r> --spawn` | Claim next task from board, route to standing agent with task packet |
| `herd up <lane-name>` | Start a single lane agent |
| `herd selftest` | Run the self-test suite |

---

## How Cards Flow

1. Cards in Kaneo with `status: to-do` are candidates
2. `herd pulse` sorts by `priority DESC, ref ASC` and claims the top match
3. The card is moved to `in-progress` via `kaneo task status <id> in-progress`
4. A structured task packet (ref, title, description, worktree path, workflow steps) is delivered to the standing agent via `herdr agent prompt`
5. The agent works the task, commits, and either moves the card forward or signals for review
6. The next `herd pulse` picks up the next card

---

## Self-Forging Architecture

Herdforge uses itself to build itself. Every package in the [ownership grid](AGENTS.md#2-complete-package-ownership-grid-30-packages) can be developed by a worker agent spawned from `herd pulse`, reviewed by a reviewer agent, and merged — all while the forge runs on cheap local inference (deepseek-v4-flash via opencode).

There are no API keys required for Kaneo when `use_cli: true` — the `kaneo` CLI handles authentication via its configured config file.

---

## License

MIT
