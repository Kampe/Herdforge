# Herdforge Agentic Worker Contract

This contract governs all AI agents (Claude, Codex, Grok, AGY, and OpenCode) participating in the **Herdforge** software factory fleet.

---

## 1. Core Engineering Invariants & Rules

0. **Fleet-Only Forging (FAC-113, hard invariant)**:
   - Herdforge forges itself with its own **Herdr fleet** (configured vendor harnesses in Herdr tabs, driven by `herd dispatch`/`herd send`/`herd review`). This is the entire point of the repo: the product must be exercised on itself.
   - **NEVER** use Claude subagents (the Agent tool: general-purpose, Explore, etc.) to build, fix, review, or audit anything in this repo. Using them defeats the dogfooding premise and stops testing the actual system.
   - The coordinator dispatches to the fleet and owns git plumbing (harvest → PR → merge → `herd approve`). Fleet agents commit in their worktree and report; they never push or merge. When an agent stalls, the fix is a firmer packet / `herd kick` / the completion self-gate (`herd verify`), never routing around the fleet.

1. **Fail-Closed Execution**:
   - All CLI tools and internal package functions **must propagate non-zero exit codes** on error (`os.Exit(1)` or non-nil `error`).
   - HTTP responses carrying `{"error": ...}` JSON bodies returned under `200 OK` **must be rejected as hard errors**.

2. **Mutation & Test Non-Vacuity Discipline**:
   - Never write a test that passes vacuously.
   - Any negative assertion guard **must be manually or programmatically verified failing** against an introduced regression before PR approval.

3. **Repo-Relative Path Enforcement**:
   - Never output absolute file paths (e.g. `/Users/...` or `/tmp/...`) inside configuration files, git hooks, or generated artifacts.
   - Use `./` or runtime environment variables (`$HERD_ROOT`, `$HERD_WORKTREE`) to ensure worktree portability.

4. **Deterministic Task Selection**:
   - When listing candidate tasks from Kaneo, GitHub, Linear, Jira, or Azure DevOps, candidate arrays **must be sorted deterministically**:
     $$\text{Sort Order} = (\text{Priority DESC}, \text{Ticket Number ASC})$$
   - Agents must check that role labels match their assigned persona (`pulse --role <role>`) before claiming work.

5. **Sized-to-Risk Cross-Model Review**:
   - **Mechanical/R0 (Docs, Markdown, Unit Tests)**: May be auto-merged via AST / deterministic test verification.
   - **Code/R1-R3 (Core Logic, Infrastructure, Auth)**: Must be reviewed and approved by a **different model family** than the author (e.g., Anthropic author $\rightarrow$ Gemini reviewer).

6. **Test Hermeticity (FAC-215)**:
   - CI runs on a clean Linux box with no installed AI CLIs, no Docker daemon in the unit gate, and an older Docker daemon in the hermetic gate. Six consecutive CI breaks in one session were all tests that asserted a property of the machine they ran on.
   - `exec.LookPath` in a test **must** be followed by `t.Skip`/`t.Skipf` on error — never `t.Fatal`. A missing binary is an environment difference, not a test failure.
   - `exec.Command` on a binary not guaranteed on CI (docker, pg_ctl, psql, codex, claude, grok, herdr, python3, jq, etc.) **must** be preceded by a `LookPath`+`t.Skip` guard in the same function. `git`, `go`, `sh`, `echo`, `sleep` are always present and exempt.
   - Never assert `argv[N] == "--flag"` at a fixed index `N > 0`. Search for the flag by iterating: `for i := range argv { if argv[i] == "--flag" { ... } }`. Position is a property of one vendor's command line; presence is the property the gate is about. Suppress a legitimate fixed-contract assertion with `//hermetic:allow-argv-position`.
   - Never use `docker image inspect --platform` — the flag is Docker 28+ only. Pass `--platform` to `docker pull` only, and verify the architecture from the inspect output.
   - Enforced by `go run ./scripts/hermeticity/` in `make lint`.

---

## 2. Core Package Ownership Grid (30 of 99 packages)

The grid below covers the 30 load-bearing packages an agent touches most. It is
deliberately not exhaustive — `ls -d pkg/*/` is the authoritative list, and
`make test-unit` runs all of them. Do not infer that a package is absent from
the tree because it is absent from this table.

| Package | Purpose & Scope | Primary Test Target |
| :--- | :--- | :--- |
| `cmd/herd` | CLI entry point & subcommand router (`init`, `preflight`, `selftest`, `status`, `pulse`, `sh`) | `go test ./cmd/herd` |
| `pkg/budget` | Dollar ($USD) and token spend governance manager | `go test ./pkg/budget` |
| `pkg/claim` | Atomic task claiming & mutex scope lock manager | `go test ./pkg/claim` |
| `pkg/config` | Declarative `.herd/herd.yaml` parser & validator | `go test ./pkg/config` |
| `pkg/conflict` | LLM semantic git merge conflict resolver | `go test ./pkg/conflict` |
| `pkg/cron` | Distributed agent scheduled task cron engine | `go test ./pkg/cron` |
| `pkg/daemon` | Core orchestration daemon & heartbeat engine | `go test ./pkg/daemon` |
| `pkg/gc` | Ephemeral git worktree garbage collector | `go test ./pkg/gc` |
| `pkg/graph` | Multi-repo workspace dependency graph engine | `go test ./pkg/graph` |
| `pkg/harness` | Universal AI harness adapter (`claude`, `codex`, `opencode`, `grok`, `agy`) | `go test ./pkg/harness` |
| `pkg/llm` | Local Ollama and vLLM provider adapter | `go test ./pkg/llm` |
| `pkg/mail` | Inter-agent durable JSONL mailbox protocol | `go test ./pkg/mail` |
| `pkg/memory` | Knowledge-graph & error pattern session memory store | `go test ./pkg/memory` |
| `pkg/metrics` | Prometheus metrics exporter (`/metrics`) | `go test ./pkg/metrics` |
| `pkg/notifier` | Multi-platform notifier (Slack, Discord, Teams) | `go test ./pkg/notifier` |
| `pkg/plugin` | WASM verifier plugin execution engine | `go test ./pkg/plugin` |
| `pkg/preflight` | Workspace boundary & absolute path leak scanner | `go test ./pkg/preflight` |
| `pkg/provider` | Pluggable task engine (Kaneo, Linear, GitHub, Jira, Azure, Memory) | `go test ./pkg/provider` |
| `pkg/release` | Conventional commit log parser & CHANGELOG generator | `go test ./pkg/release` |
| `pkg/review` | Adversarial risk classification & review pipeline | `go test ./pkg/review` |
| `pkg/router` | Multi-provider LLM load balancer & 429 cooldowns | `go test ./pkg/router` |
| `pkg/security` | Secret-scanning & credential leak guardrails | `go test ./pkg/security` |
| `pkg/selftest` | Self-test assertion engine & boundary runner | `go test ./pkg/selftest` |
| `pkg/server` | OpenAPI 3.0 REST control server (`/v1/status`) | `go test ./pkg/server` |
| `pkg/skill` | WASM sandboxed dynamic skill runner | `go test ./pkg/skill` |
| `pkg/sync` | Multi-board state reconciliation engine | `go test ./pkg/sync` |
| `pkg/tui` | Live fleet operations dashboard & REPL shell (`herd sh`) | `go test ./pkg/tui` |
| `pkg/verifier` | Language-agnostic test harness runner | `go test ./pkg/verifier` |
| `pkg/webhook` | Event-driven HMAC webhook receiver engine | `go test ./pkg/webhook` |
| `pkg/worker` | Worker lane process supervisor | `go test ./pkg/worker` |
| `pkg/worktree` | Git worktree creation, isolation, & pruning | `go test ./pkg/worktree` |

---

## 3. Workflow for Agentic Tasks

1. **Preflight**: Run `make preflight` in your isolated worktree before editing.
2. **Implementation**: Modify files in your assigned worktree; keep changes atomic and focused on the claimed task.
3. **Verification**: Run `make lint all` and ensure zero test failures.
4. **Commit**: Format commit message using Conventional Commits (`feat: ...`, `fix: ...`, `docs: ...`).

<!-- code-review-graph CLI -->
## Code intelligence: code-review-graph CLI

**IMPORTANT: This project has a knowledge graph. ALWAYS use the
`code-review-graph` CLI BEFORE using Grep/Glob/Read to explore
the codebase.** The graph is faster, cheaper (fewer tokens), and gives
you structural context (callers, dependents, test coverage) that file
scanning cannot.

### When to use graph tools FIRST

- **Exploring code**: `semantic_search_nodes_tool` or `query_graph_tool` instead of Grep
- **Understanding impact**: `get_impact_radius_tool` instead of manually tracing imports
- **Code review**: `detect_changes_tool` + `get_review_context_tool` instead of reading entire files
- **Finding relationships**: `query_graph_tool` with callers_of/callees_of/imports_of/tests_for
- **Architecture questions**: `get_architecture_overview_tool` + `list_communities_tool`

Fall back to Grep/Glob/Read **only** when the graph doesn't cover what you need.

### Key Tools

| Tool | Use when |
| ------ | ---------- |
| `detect_changes_tool` | Reviewing code changes — gives risk-scored analysis |
| `get_review_context_tool` | Need source snippets for review — token-efficient |
| `get_impact_radius_tool` | Understanding blast radius of a change |
| `get_affected_flows_tool` | Finding which execution paths are impacted |
| `query_graph_tool` | Tracing callers, callees, imports, tests, dependencies |
| `semantic_search_nodes_tool` | Finding functions/classes by name or keyword |
| `get_architecture_overview_tool` | Understanding high-level codebase structure |
| `refactor_tool` | Planning renames, finding dead code |

### Workflow

1. The graph auto-updates on file changes (via hooks).
2. Use `detect_changes_tool` for code review.
3. Use `get_affected_flows_tool` to understand impact.
4. Use `query_graph_tool` pattern="tests_for" to check coverage.
