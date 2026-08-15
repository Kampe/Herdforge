# CLAUDE.md - Build Bootstrap & Routing Protocol

This document serves as the build bootstrap and routing specification for Claude and peer AI agents operating over **Herdforge**.

---

## 1. Quick Invariants (Hard Rules)

1. **Origin Main is Truth**: Development occurs in worktrees or feature branches; `main` is always clean and green.
2. **Fail-Closed Execution**: Subcommands and API wrappers MUST exit non-zero on error. HTTP 200 responses containing `{"error": ...}` JSON bodies are hard errors.
3. **No Absolute Paths**: Absolutely NO absolute paths in code, configs, or markdown. Use `./` or `$HERD_ROOT`.
4. **Deterministic Sorting**: Candidate task queues MUST be sorted by $(\text{Priority DESC}, \text{Ticket Ref ASC})$.
5. **No AI Trailers in Commits**: Never add `Co-Authored-By`, `Signed-off-by`, or AI attribution trailers to git commits.

---

## 2. Fast Commands

```bash
# Build binary
make build

# Run preflight, unit tests, and build
make all

# Run self-test suite against active repository
make self-test

# Run unit tests across all packages
make test-unit

# Run preflight workspace boundary check
make preflight

# Launch interactive REPL shell
./bin/herd sh

# Execute pulse task claim sweep
./bin/herd pulse --role worker
```

---

## 3. Topic Routing Matrix

| Topic | Reference Document |
| :--- | :--- |
| **Agent Skill & Integration Guide** | [SKILL.md](SKILL.md) / [.herd/skills/herd.md](.herd/skills/herd.md) |
| **Agent Prompts & Contracts** | [.herd/prompts/](.herd/prompts/) |
| **Agent Governance & Invariants** | [AGENTS.md](AGENTS.md) |
| **Architecture & API Specs** | [docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md](docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md) |
| **System RFC & Design** | [docs/rfcs/RFC-001-HERD-DAEMON.md](docs/rfcs/RFC-001-HERD-DAEMON.md) |

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
