# Herdforge

**Herdforge** is a standalone, repo-agnostic multi-agent orchestration daemon and software factory CLI written in Go. It turns any Git repository into an autonomous agentic development environment with multi-provider AI model routing, git worktree isolation, automated verification, and independent cross-model review pipelines.

[![CI Workflow](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml/badge.svg)](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/Kampe/Herdforge.svg)](https://pkg.go.dev/github.com/Kampe/Herdforge)

---

## 🛠️ Key Features

- ⚙️ **Declarative Config (`herd.yaml`)**: Initialize any codebase with `herd init` and define worker roles, model providers, and task backends.
- 🔌 **Pluggable Task Engine**: Native support for **Kaneo**, **Linear**, **GitHub Issues**, and offline **Memory** task providers.
- 🔀 **Multi-Provider AI Router**: Dynamic load-balancing and 429 rate-limit failover across OpenAI, Anthropic, Gemini, and local Ollama/vLLM models.
- 🌳 **Worktree & Lane Isolation**: Ephemeral Git worktree management to isolate parallel agent lanes without workspace file collisions.
- 🛡️ **Preflight Boundary Check**: Automatic detection of absolute path leaks to ensure worktree portability.
- 🧪 **Mutation Verification**: Language-agnostic test harness runner (`npm`, `pytest`, `cargo`, `go test`) with mutation testing to kill vacuous agent tests.
- 🔍 **Independent Cross-Model Review**: Risk-based code review pipeline enforcing cross-family model reviews before git rebase-merging.

---

## 📋 Declarative Configuration Schema (`.herd/herd.yaml`)

Initialize your codebase by placing a `.herd/herd.yaml` configuration file at the repository root:

```yaml
version: "1"

# Project identification
project:
  name: "my-herd-app"
  default_branch: "main"

# Task / Issue Tracker integration
# Supported types: kaneo | linear | github | memory
task_provider:
  type: "kaneo"
  project_id: "b939c5jzixruza3vvywrg1hs"
  api_url: "https://kanban-api.kampe.kluster"

# LLM Providers for multi-model failover & rate-limit (429) cooldowns
model_providers:
  - name: "claude-pro"
    type: "anthropic"
    model: "claude-3-7-sonnet"
  - name: "gemini-flash"
    type: "google"
    model: "gemini-2.5-flash"
  - name: "codex-local"
    type: "openai"
    model: "gpt-4o"

# Worker roles & prompt routing
roles:
  - name: "herd-smith"
    provider: "claude-pro"
    fallback_provider: "gemini-flash"
    prompt_path: ".herd/prompts/smith.md"
  - name: "reviewer"
    provider: "gemini-flash"
    fallback_provider: "codex-local"
    prompt_path: ".herd/prompts/reviewer.md"

# Project-native test harness verification
verification:
  test_command: "make test"
  preflight_command: "make preflight"
```

---

## 🚀 Quickstart & Makefile Commands

```bash
# Clone repository
git clone https://github.com/Kampe/Herdforge.git
cd Herdforge

# Run Makefile (preflight, lint, tests, build)
make all

# Scaffold default .herd/herd.yaml in any repo
go run ./cmd/herd init

# Execute preflight boundary verification check
go run ./cmd/herd preflight

# Execute single orchestration sweep pass
go run ./cmd/herd pulse --role herd-smith
```

---

## 📚 Documentation & Agent Protocols

- **Agent Contract & Invariants**: [`AGENTS.md`](AGENTS.md)
- **Build Bootstrap**: [`CLAUDE.md`](CLAUDE.md)
- **Agent Implementation Guide**: [`docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md`](docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md)
- **Architecture RFC**: [`docs/rfcs/RFC-001-HERD-DAEMON.md`](docs/rfcs/RFC-001-HERD-DAEMON.md)

---

## 🏛️ Architecture Overview

```
                        ┌─────────────────────────────────┐
                        │       Task Queue Provider       │
                        │ (Kaneo / Linear / GitHub / Mem) │
                        └────────────────┬────────────────┘
                                         │
                                         ▼
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                            Herdforge Daemon                              │
 │                                                                          │
 │  ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐    │
 │  │ Task Engine      │ ── │ Worktree Manager │ ── │  Model Router    │    │
 │  └──────────────────┘    └──────────────────┘    └──────────────────┘    │
 │           │                        │                       │             │
 │           ▼                        ▼                       ▼             │
 │  ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐    │
 │  │ Test Verifier    │ ── │ Preflight Check  │ ── │ Git Rebase-Merge │    │
 │  └──────────────────┘    └──────────────────┘    └──────────────────┘    │
 └──────────────────────────────────────────────────────────────────────────┘
```

---

## 📄 License

[MIT](LICENSE)
