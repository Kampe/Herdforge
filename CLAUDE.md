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

# Run unit tests across all 30 packages
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
| **Agent Prompts & Contracts** | [examples/prompts/](examples/prompts/) / [.herd/prompts/](.herd/prompts/) |
| **Agent Governance & Invariants** | [AGENTS.md](AGENTS.md) |
| **Architecture & API Specs** | [docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md](docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md) |
| **System RFC & Design** | [docs/rfcs/RFC-001-HERD-DAEMON.md](docs/rfcs/RFC-001-HERD-DAEMON.md) |
