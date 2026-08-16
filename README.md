# Herdforge

Herdforge is a Go control plane for turning repository work queues into isolated implementation, deterministic verification, independent review, serialized integration, and reconciled board state. It uses Herdr as the agent execution plane and supports pluggable task providers; Kaneo is the checked-in default.

[![CI Workflow](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Kampe/Herdforge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Project status

Herdforge is actively self-hosting, but it is not yet an unattended merge authority. The current CLI has working primitives for provider access, deterministic selection, Herdr delivery, worktrees, verification, review evidence, fleet inspection, and board reconciliation. The durable end-to-end lifecycle is still under construction.

Use the current commands as assisted operations and keep integration visible to an operator. In particular, do not assume that `herd daemon` or the experimental one-shot `herd forge` command currently enforces every target gate.

- [Target workflow and fleet contract](docs/architecture/TARGET-WORKFLOW.md)
- [Architecture and fleet audit — 2026-08-02](docs/architecture/AUDIT-2026-08-02.md)
- [Second-pass audit and runtime integration findings — 2026-08-02](docs/architecture/AUDIT-SECOND-PASS-2026-08-02.md)
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

The lanes below are the roster in `.herd/herd.yaml`. "Standing" lanes are raised
once as control plane (`herd standing`); ephemeral lanes are launched per
dispatch and bound to one task/revision.

| Role | Lane | Standing | Purpose |
| --- | --- | --- | --- |
| Orchestrator | `orchestrator` | yes | coordinates capacity and state transitions; does not author, review, or merge |
| Scout-planner | `scout-planner` | yes | grooms the queue, identifies dependencies, and proposes deterministic eligible work |
| Review supervisor | `review-supervisor` | yes | moves completed candidates through verification and review queues |
| Harvest/integration owner | `harvest` | yes | serializes approved merges, reconciles the board, and cleans exact resources |
| Recovery sentinel | `recovery-sentinel` | yes | detects lost callbacks, stale leases, root bleed, and stranded work |
| Recovery (mender) | `mender` | yes | fixes the forge itself — the gates, evidence checks, and lifecycle bugs that cost the fleet whole sessions |
| Worker | `smith` | no | implements a bounded task in an owned worktree |
| Forge-smith | `scout` | no | surveys the board and grooms bare cards into dispatch-ready specs |
| Reviewer | `assayer` | no | performs read-only, different-family review of an exact SHA |
| Verification gate | `verification-gate` | no | runs deterministic checks and records evidence for an exact SHA |

One lane per role is enforced: `CanonicalLaneRegistry` requires it so a lane may
be resolved by either its name or its role unambiguously, and
`validateLaneLaunchConfig` accepts eleven roles, pinning each to exactly one
task shape. Adding a standing lane therefore needs a free role, not just a new
config block — which is why the forge-repair lane is named `mender` and carries
the `recovery` role. Ten of the eleven roles are occupied above; `assayer` is
the one still free.

Worker, forge-smith, and recovery lanes have checked-in Codex/Luna defaults,
but live routing may select another configured vendor harness and model. Pi is
not part of the supported fleet. Runtime model selection belongs to routing
policy; author and reviewer must not be pinned to the same model family for
R1–R3 work.

Spawn-ready prompt contracts are in `.herd/prompts/`. Runtime model selection belongs to routing policy; author and reviewer must not be pinned to the same model family for R1–R3 work.

## Requirements

- Go 1.24 or newer
- Git
- Herdr
- a configured task-provider CLI or API; Kaneo is the checked-in default
  (`task_provider.type: "kaneo"`), while Linear and other wired providers are
  opt-in through an explicit local configuration

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
herd validate-config
herd preflight
```

`herd init --full` writes a starting `.herd/herd.yaml`. Point its `task_provider`
block at your board before validating — set `type`, list that same name in
`enabled`, and set `project_id` plus the `api_key_env` naming the environment
variable that holds the token. See [Task provider](#task-provider) for which
adapters are actually selectable.

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

### Local Herdr mode

From a checkout with Herdr installed, the normal developer path is:

```sh
herd forge --loop
```

Herdforge reads the single Kanban binding in `.herd/herd.yaml`, asks the Herdr
router for the configured harness/model, and starts the agent through Herdr's
tab API. Local mode is the default and does not require provider API keys in
the agent environment, live harness-proof panels, or kernel signer setup.

Hosted control-plane deployments can opt into the stricter signer, attestation,
and MAC gates with `HERD_MODE=production` (or `HERD_CONTROL_SECRET`).

### Task provider

`.herd/herd.yaml` ships with Kaneo selected. `task_provider.enabled` is an explicit activation
allowlist (FAC-155): the factory activates `type` only if it is listed there, so anything else
can never be auto-detected, probed, or selected. Changing `type` without moving that list fails
closed before any board read or mutation.

Adapter availability comes in two tiers, and the allowlist only governs the first:

- **Wired into the factory** (`pkg/provider/factory.go`) — `linear`, `kaneo`, `memory`. Selectable
  by adding the name to `enabled` and setting `type`.
- **Adapter code present but not wired** — `jira`, `azure`, `github`. These have no factory case,
  so selecting one fails with `task_provider.type %q is not activated in this build`
  (`factory.go:90`) even when it is allowlisted. Wiring them up is outstanding work, not config.

To run against a different project without touching the checked-in config, copy the
credential-free example into the ignored local profile and select it at runtime:

```bash
cp docs/examples/herd.linear.yaml .herd/herd.yaml.local
export LINEAR_API_KEY="..."
HERD_CONFIG_PATH=.herd/herd.yaml.local herd validate-config
HERD_CONFIG_PATH=.herd/herd.yaml.local herd status
```

The `linear` provider requires `task_provider.api_key_env` and fails closed when that environment variable is empty; it never falls back to `KANEO_API_KEY`. Linear transitions resolve the requested canonical status to a workflow state ID for the task's team, then read the issue back after mutation. Do not commit the local profile with a token.

Do not treat the model on a lane as permanent identity. Routing must distinguish:

- execution harness/backend;
- provider account or quota pool;
- concrete model;
- model family used for review independence;
- tool and write capability proven by probes.

### Worker launch policy (FAC-175)

Every implementation, repair, and recovery worker launch must carry one complete routed decision: role, task shape, provider, model, effort, provenance, and argv. The compiled launch boundary accepts only `codex --model gpt-5.6-luna -c model_reasoning_effort=medium -a never` for worker/forge-smith/recovery roles. Bare launches, coordinator shapes, `gpt-5.6-sol`, `gpt-5.6-terra`, `claude-fable-5`, missing effort, and missing provenance fail before tab/process/prompt or board/worktree mutation. Rejections are recorded in the repo-relative `.herd/launch-receipts.jsonl` (override with `HERD_LAUNCH_RECEIPTS`).

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
