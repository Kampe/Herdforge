# Herdforge Agentic Worker Contract

This contract governs all AI agents (Claude, Gemini, Codex, Grok, Ollama) participating in the **Herdforge** software factory fleet.

---

## 1. Core Engineering Invariants & Rules

1. **Fail-Closed Execution**:
   - All CLI tools and internal package functions **must propagate non-zero exit codes** on error (`os.Exit(1)` or non-nil `error`).
   - HTTP responses carrying `{"error": ...}` JSON bodies returned under `200 OK` **must be rejected as hard errors**.

2. **Mutation & Test Non-Vacuity Discipline**:
   - Never write a test that passes vacuously.
   - Any negative assertion guard **must be manually or programmatically verified failing** against a introduced regression before PR approval.

3. **Repo-Relative Path Enforcement**:
   - Never output absolute file paths (e.g. `/Users/...` or `/tmp/...`) inside configuration files, git hooks, or generated artifacts.
   - Use `./` or runtime environment variables (`$HERD_ROOT`, `$HERD_WORKTREE`) to ensure worktree portability.

4. **Deterministic Task Selection**:
   - When listing candidate tasks from Kaneo, GitHub, or Linear, candidate arrays **must be sorted deterministically**:
     $$\text{Sort Order} = (\text{Priority DESC}, \text{Ticket Number ASC})$$
   - Agents must check that role labels match their assigned persona (`next --role <role>`) before claiming work.

5. **Sized-to-Risk Cross-Model Review**:
   - **Mechanical/R0 (Docs, Markdown, Unit Tests)**: May be auto-merged via AST / deterministic test verification.
   - **Code/R1-R3 (Core Logic, Infrastructure, Auth)**: Must be reviewed and approved by a **different model family** than the author (e.g., Anthropic author $\rightarrow$ Gemini reviewer).

---

## 2. Package Ownership Grid

| Package | Purpose & Scope | Primary Tests |
| :--- | :--- | :--- |
| `cmd/herd` | CLI entry point and subcommand router | `go test ./cmd/herd/...` |
| `pkg/config` | `herd.yaml` declarative configuration parser & validator | `go test ./pkg/config/...` |
| `pkg/provider` | Unified Task Engine interface (Kaneo, GitHub, Linear) | `go test ./pkg/provider/...` |
| `pkg/router` | Multi-provider LLM load balancer & 429 rate-limit fallback | `go test ./pkg/router/...` |
| `pkg/worktree` | Git worktree creation, isolation, & auto-pruning | `go test ./pkg/worktree/...` |
| `pkg/verifier` | Test harness runner & stack trace parser | `go test ./pkg/verifier/...` |

---

## 3. Workflow for Agentic Tasks

1. **Preflight**: Run `go test ./...` in your isolated worktree before editing.
2. **Implementation**: Modify files in your assigned worktree; keep changes atomic and focused on the claimed task.
3. **Verification**: Run `go test ./...` and ensure zero test failures.
4. **Commit**: Format commit message using Conventional Commits (`feat: ...`, `fix: ...`, `docs: ...`).
