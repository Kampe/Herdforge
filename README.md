# Herdforge

**Herdforge** is a standalone, repo-agnostic multi-agent orchestration daemon and software factory CLI. It turns any Git repository into an autonomous agentic development environment with multi-provider AI model routing, git worktree isolation, automated verification, and independent model review pipelines.

---

## Key Features

- ⚙️ **Declarative Config (`herd.yaml`)**: Initialize any codebase with `herd init` and define worker roles, model providers, and task backends.
- 🔀 **Multi-Provider AI Router**: Dynamic load-balancing and 429 rate-limit failover across OpenAI, Anthropic, Gemini, Codex, and local Ollama/vLLM models.
- 🌳 **Worktree & Lane Isolation**: Ephemeral Git worktree management to isolate parallel agent lanes without workspace file collisions.
- 🛡️ **Automated Verification**: Language-agnostic test harness runner (`npm`, `pytest`, `cargo`, `go test`) with stack trace feedback.
- 🔍 **Independent Cross-Model Review**: Risk-based code review pipeline enforcing cross-family model reviews before git rebase-merging.
- 📊 **Live TUI & Status Dashboard**: Real-time terminal interface (`herd status`) displaying active workers, branch states, and token/quota burn.

---

## Quickstart

```bash
# Initialize Herdforge in your project
herd init

# Start the background orchestration daemon
herd daemon --start

# Check live fleet status
herd status
```

---

## Architecture Overview

```
                        ┌──────────────────────────────┐
                        │      Kanban / Task Queue     │
                        │  (Kaneo / GitHub / Linear)   │
                        └──────────────┬───────────────┘
                                       │
                                       ▼
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                            Herdforge Daemon                              │
 │                                                                          │
 │  ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐    │
 │  │ Task Dispatcher  │ ── │ Worktree Manager │ ── │  Model Router    │    │
 │  └──────────────────┘    └──────────────────┘    └──────────────────┘    │
 │           │                        │                       │             │
 │           ▼                        ▼                       ▼             │
 │  ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐    │
 │  │ Test Harness     │ ── │ Review Engine    │ ── │ Git Rebase-Merge │    │
 │  └──────────────────┘    └──────────────────┘    └──────────────────┘    │
 └──────────────────────────────────────────────────────────────────────────┘
```

---

## License

[MIT](LICENSE)
