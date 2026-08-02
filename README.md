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
- 🔍 **Independent Cross-Model Review**: Risk-based code review pipeline enforcing cross-family model reviews before git rebase-merging.

---

## 🚀 Quickstart

```bash
# Clone repository
git clone https://github.com/Kampe/Herdforge.git
cd Herdforge

# Run preflight verification check
go run ./cmd/herd preflight

# Scaffold default .herd/herd.yaml configuration
go run ./cmd/herd init

# Run test suite
go test ./...
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
