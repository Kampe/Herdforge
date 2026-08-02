---
name: herd
description: Operate the Herdforge multi-agent software factory daemon and CLI. Use for inspecting orchestration status, running self-test suites, pulse sweeps, task claiming, budget checks, and worktree lane management.
---

# Herdforge Agent Skill Guide

This skill equips AI agents (Claude, Codex, Gemini, OpenCode, Antigravity) to operate the compiled `herd` CLI and daemon inside any repository.

## Commands Reference

### 1. Preflight Workspace Boundary Check
Verify that no absolute file paths leak into git tracking or config files:
```bash
herd preflight
```

### 2. Core Self-Test Suite
Run the compiled self-test assertion suite against the active repo:
```bash
herd selftest
```

### 3. Orchestration Status
Inspect the active task provider, project ID, and configured worker roles:
```bash
herd status
```

### 4. Orchestration Pulse Sweep
Execute one pulse sweep pass to list candidate tasks from Kaneo, Linear, GitHub, Jira, or Azure DevOps, claim the highest-priority task, and raise worker lanes:
```bash
herd pulse --role worker
```

### 5. Interactive REPL Shell Mode
Launch an interactive REPL shell for live operator inspection (`status`, `lanes`, `budget`, `claim <ref>`):
```bash
herd sh
```

### 6. Repository Initialization
Scaffold a default `.herd/herd.yaml` configuration file in any new Git repository:
```bash
herd init
```

## Agent Worktree Invariants
- Always run `herd preflight` before staging or committing changes.
- Ensure every feature branch passes `make lint all` or `herd selftest`.
- Never write hardcoded absolute user paths. All paths must be repository-relative.
