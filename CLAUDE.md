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
go build -o bin/herd ./cmd/herd

# Run unit test suite
go test ./...

# Run preflight workspace boundary check
go run ./cmd/herd preflight

# Execute test sweep for specific package
go test ./pkg/daemon/...
```

---

## 3. Topic Routing Matrix

| Topic | Reference Document |
| :--- | :--- |
| **Agent Contracts & Standards** | [AGENTS.md](AGENTS.md) |
| **Architecture & API Specs** | [docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md](docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md) |
| **System RFC & Design** | [docs/rfcs/RFC-001-HERD-DAEMON.md](docs/rfcs/RFC-001-HERD-DAEMON.md) |
