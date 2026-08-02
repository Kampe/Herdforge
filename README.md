# Herdforge

Herdforge is a Go control plane for turning repository work queues into isolated implementation, deterministic verification, independent review, serialized integration, and reconciled board state. It uses Herdr as the agent execution plane and supports pluggable task providers such as Kaneo.

[![CI Workflow](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Project status

Herdforge is actively self-hosting, but it is not yet an unattended merge authority. The current CLI has working primitives for provider access, deterministic selection, Herdr delivery, worktrees, verification, review evidence, fleet inspection, and board reconciliation. The durable end-to-end lifecycle is still under construction.

Use the current commands as assisted operations and keep integration visible to an operator. In particular, do not assume that `herd daemon` or the experimental one-shot `herd forge` command currently enforces every target gate.

- [Target workflow and fleet contract](docs/architecture/TARGET-WORKFLOW.md)
- [Architecture and fleet audit — 2026-08-02](docs/architecture/AUDIT-2026-08-02.md)
- [Technical implementation guide](docs/architecture/AGENT-IMPLEMENTATION-GUIDE.md)

## The intended loop

```text
groom → claim/lease → isolated dispatch → commit → verify
      → different-family review → serialized integration
      → origin/main proof → board reconciliation → cleanup → backfill
```

Every arrow is an idempotent, evidence-backed state transition. The board is an operator view, while Git revisions, verification artifacts, review ledger records, claim leases, and delivery receipts form the execution evidence.

“No idle work” means that eligible work is backfilled immediately when safe capacity exists. It does not mean dispatching vague cards, exceeding review capacity, reusing the shared checkout, or weakening model-family independence.

## Herdforge and Herdr

Herdforge owns repository policy:

- task eligibility, role matching, dependency checks, and deterministic order;
- claims, leases, task worktrees, and revision identity;
- verification, review, integration, and board reconciliation;
- recovery policy and audit history.

Herdr owns portable process mechanics:

- agent sessions and terminal addressing;
- prompt delivery and consumption;
- provider/model availability and quota posture;
- process attention, interruption, and cleanup signals.

The project deliberately composes with Herdr instead of copying every Chainseer shell behavior into Go.

## Fleet roles

The target fleet is not a fixed three-agent conveyor belt. Control roles may be standing; task roles should normally be ephemeral and bound to one task/revision.

| Role | Purpose |
| --- | --- |
| Orchestrator | coordinates capacity and state transitions; does not author, review, or merge |
| Scout-planner | grooms the queue, identifies dependencies, and proposes deterministic eligible work |
| Worker | implements a bounded task in an owned worktree |
| Smith | handles larger or higher-risk implementation in an owned worktree |
| Verification gate | runs deterministic checks and records evidence for an exact SHA |
| Reviewer | performs read-only, different-family review of an exact SHA |
| Review supervisor | moves completed candidates through verification and review queues |
| Harvest/integration owner | serializes approved merges, reconciles the board, and cleans exact resources |
| Recovery sentinel | detects lost callbacks, stale leases, root bleed, and stranded work |

Spawn-ready prompt contracts are in `.herd/prompts/`. Runtime model selection belongs to routing policy; author and reviewer must not be pinned to the same model family for R1–R3 work.

## Requirements

- Go 1.24 or newer
- Git
- Herdr
- a configured task-provider CLI or API; Kaneo is the primary current adapter

## Build and inspect

```bash
make ci
make build
./bin/herd status
./bin/herd --help
```

`make ci` is the repository’s hermetic pre-push gate. It runs `go vet`, the full unit suite, and repo-boundary preflight without depending on user Git signing configuration.

## Repository setup

In a repository that will be managed by Herdforge:

```bash
herd init --full
kaneo link -w <workspace-id> -p <project-id>
herd validate-config
herd preflight
```

Review the generated `.herd/herd.yaml` and prompts before launching agents. A dispatchable board card needs acceptance criteria, a role mapping, dependency state, risk information, and operator-owned priority.

## Assisted operation

Useful read-only and bounded commands include:

```bash
herd status
herd next
herd attention
herd resources
herd throughput
herd worktrees
herd overlap
herd unmerged --all
herd board-sync
```

Task operations should be run with their help output and target evidence checked explicitly:

```bash
herd pulse --role worker --spawn
herd dispatch FAC-123
herd review --spawn
herd board-done FAC-123
```

The exact command surface is evolving quickly while the Chainseer workflow is ported. `herd --help` is authoritative for the binary at the checked-out revision.

## Board lifecycle

Provider status names normalize to:

```text
to-do → in-progress → in-review → done
```

Those columns are not sufficient evidence by themselves:

- `to-do` is eligible only after grooming and dependency checks;
- `in-progress` means a live claim/lease, not that an agent merely received text;
- `in-review` requires a committed candidate and passing verification receipt;
- `done` requires proof on `origin/main` and provider readback.

Unknown provider state, failed delivery, stale review SHA, same-family review, dirty shared checkout, or missing merge evidence must stop the transition.

## Configuration

The repository-local config lives at `.herd/herd.yaml` and describes the project, task provider, lanes, routing candidates, and verification commands. Paths stored in configuration and generated artifacts must remain repository-relative.

Do not treat the model on a lane as permanent identity. Routing must distinguish:

- execution harness/backend;
- provider account or quota pool;
- concrete model;
- model family used for review independence;
- tool and write capability proven by probes.

Run `herd init --full` to generate a schema-compatible starting point, then review it against the checked-out binary with `herd validate-config`.

## Safety invariants

- Agents work only in their assigned task worktree; the shared checkout is coordinator/integration-only.
- Claims and lifecycle transitions fail closed and are idempotent.
- Every candidate is identified by an immutable commit SHA and protected ref.
- Verification cannot pass vacuously; required negative assertions must be shown to fail under regression.
- R1–R3 work is reviewed by a different model family from the author.
- Only one integration owner mutates the default branch at a time.
- Board `done` follows `origin/main` proof, never pane or branch state.
- Unique dirty or committed work is never removed during cleanup.

## Development

```bash
make preflight
make lint
make test-unit
make ci
```

Changes follow Conventional Commits and the binding rules in [AGENTS.md](AGENTS.md). Inspect the code graph with the `code-review-graph` CLI before broad source scanning.

## License

MIT
