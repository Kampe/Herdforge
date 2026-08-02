# Herdforge

**Herdforge** is a standalone, repo-agnostic multi-agent orchestration daemon and software factory CLI written in Go. It turns any Git repository into an autonomous agentic development environment with multi-provider AI model routing, git worktree isolation, automated verification, and independent cross-model review pipelines.

[![CI Workflow](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/Kampe/Herdforge.svg)](https://pkg.go.dev/github.com/Kampe/Herdforge)

---

## Key Features

- ⚙️ **Declarative Config (`herd.yaml`)**: Initialize any codebase with `herd init` and define worker roles, model providers, and task backends.
- 🔌 **Enterprise Task Engine**: Native support for **Kaneo**, **Linear**, **GitHub Issues**, **Jira Software (REST v3)**, **Azure DevOps Boards**, and offline **Memory** task providers.
- 🔀 **Multi-Provider AI Router**: Dynamic load-balancing and 429 rate-limit failover across OpenAI, Anthropic, Gemini, local Ollama, and vLLM models.
- ⚡ **Event-Driven Webhook Ingestion**: HMAC-signed HTTP webhook receiver (`pkg/webhook`) for sub-second real-time agent task dispatch.
- 💰 **Agent Spend Governance**: Dollar ($USD) and token budget manager (`pkg/budget`) with automated exhaustion worker pausing.
- 🔀 **Semantic Merge Conflict Solver**: Automated LLM git merge conflict resolver (`pkg/conflict`) for parallel agent branch reconciliation.
- 🖥️ **Interactive REPL Shell**: Operator terminal REPL shell (`herd sh` / `pkg/tui`) for live inspection and lane steering.
- ⏰ **Scheduled Maintenance Cron**: Native 5-field cron engine (`pkg/cron`) for recurring security scans and worktree GC.
- 📢 **Multi-Platform Notifier**: Real-time alert notifications (`pkg/notifier`) to Slack, Discord, and Microsoft Teams.
- 📦 **WASM Sandboxed Skills**: WASM runtime dynamic skill runner (`pkg/skill`) for isolated tool execution.
- 🌳 **Worktree & Lane Isolation**: Ephemeral Git worktree management to isolate parallel agent lanes without workspace file collisions.
- 🛡️ **Preflight Boundary Check**: Automatic detection of absolute path leaks to ensure worktree portability.
- 🧪 **Mutation Verification**: Language-agnostic test harness runner (`npm`, `pytest`, `cargo`, `go test`) with mutation testing to kill vacuous agent tests.
- 🔍 **Independent Cross-Model Review**: Risk-based code review pipeline enforcing cross-family model reviews before git rebase-merging.

---

## Dependencies & Requirements

- **Go 1.24+** — `go.mod` requires 1.24; install via `brew install go` or `mise install go`
- **Git** — worktree isolation and rebase-merge pipeline
- **Make** — `make all` drives preflight, lint, test, and build

---

## Declarative Configuration Schema (`.herd/herd.yaml`)

Initialize your codebase by placing a `.herd/herd.yaml` configuration file at the repository root:

```yaml
version: "1"

# Project identification
project:
  name: "my-herd-app"
  default_branch: "main"

# Task / Issue Tracker integration
# Supported types: kaneo | linear | github | jira | azure | memory
task_provider:
  type: "kaneo"
  project_id: "b939c5jzixruza3vvywrg1hs"
  api_url: "https://kanban-api.kampe.kluster"

# LLM Providers for multi-model failover & rate-limit (429) cooldowns
model_providers:
  - name: "claude-pro"
    type: "anthropic"
    model: "claude-3-5-sonnet"
  - name: "gemini-flash"
    type: "google"
    model: "gemini-1.5-pro"
  - name: "codex-local"
    type: "openai"
    model: "gpt-4o"

# Worker roles & prompt routing
roles:
  - name: "worker"
    provider: "claude-pro"
    fallback_provider: "gemini-flash"
    prompt_path: ".herd/prompts/worker.md"
  - name: "reviewer"
    provider: "gemini-flash"
    fallback_provider: "codex-local"
    prompt_path: ".herd/prompts/reviewer.md"

# Project-native test harness verification
verification:
  test_command: "make lint all"
```

---

## Quickstart & Commands

```bash
# Clone repository
git clone https://github.com/Kampe/Herdforge.git
cd Herdforge

# Run Makefile (preflight, lint, tests, build)
make all

# Run self-test suite against active repo
make self-test

# Scaffold default .herd/herd.yaml in any repo
./bin/herd init

# Execute preflight boundary verification check
./bin/herd preflight

# Execute single orchestration pulse sweep
./bin/herd pulse --role worker

# Launch interactive REPL shell
./bin/herd sh
```

---

## Agent Skill & Integration

Herdforge includes a native agent skill file [`SKILL.md`](SKILL.md) and prompt contracts in [`examples/prompts/`](examples/prompts/) for seamless integration with AI coding agents (Claude, Codex, Gemini, OpenCode, Antigravity).

---

## License

[MIT](LICENSE)
