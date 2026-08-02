# RFC-001: Herdforge Daemon Architecture & CLI Specification

- **Status**: Draft
- **Authors**: Kampe & Antigravity (DeepMind Advanced Agentic Coding)
- **Target Repo**: `Herdforge`

---

## Executive Summary

Herdforge is a compiled, repo-agnostic multi-agent orchestration daemon and CLI tool (`herd`). It automates software development workflows across any Git repository by coordinating task queues (Kaneo, GitHub Issues, Linear), isolating worker execution into ephemeral Git worktrees, routing LLM requests dynamically across AI providers, executing test verification harnesses, and orchestrating independent cross-model review pipelines before git rebase-merging.

---

## 1. System Components

### 1.1 `herd.yaml` Configuration Schema
The root configuration file (`.herd/herd.yaml`) defines project worker roles, model providers, verification test commands, and task queue credentials.

```yaml
version: "1"
project:
  name: "my-app"
  default_branch: "main"

task_provider:
  type: "kaneo" # kaneo | github | linear
  project_id: "b939c5jzixruza3vvywrg1hs"
  api_url: "https://kanban-api.kampe.kluster"

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

roles:
  - name: "builder"
    provider: "claude-pro"
    fallback_provider: "gemini-flash"
    prompt_path: ".herd/prompts/builder.md"
  - name: "reviewer"
    provider: "gemini-flash"
    fallback_provider: "codex-local"
    prompt_path: ".herd/prompts/reviewer.md"

verification:
  test_command: "npm test"
  preflight_command: "bin/agent-preflight"
```

---

## 2. CLI Interface (`herd`)

- `herd init`: Scaffolds `.herd/herd.yaml` and default prompts.
- `herd daemon`: Runs the background reactive event loop and state manager.
- `herd status`: Displays the interactive Terminal UI (TUI) live fleet dashboard.
- `herd claim`: Claims next prioritized task from the task queue.
- `herd verify`: Runs the project-native test harness and AST checks.
- `herd review`: Dispatches independent cross-model code review.
- `herd harvest`: Rebase-merges approved feature branches into default branch.

---

## 3. Core Architecture Guarantees & Engineering Contracts

1. **Fail-Closed Execution**: All CLI verbs and API wrappers propagate non-zero exit codes; HTTP errors wrapped in 200 OK are rejected.
2. **Mutation Verification**: All negative test assertions must be verified failing against a deliberately introduced regression before PR approval.
3. **Repo-Relative Worktree Isolation**: Ephemeral Git worktree lanes strictly enforce repo-relative paths (`./`) to prevent workspace file collisions.
4. **Deterministic Task Selection**: Task selection queries are sorted deterministically `(priority DESC, ticket_number ASC)` with explicit role-label matching.
5. **Sized-to-Risk Independent Review**: Mechanical gates (R0) handle docs/test diffs; independent cross-model family reviews (R1-R3) gate core code merges.
