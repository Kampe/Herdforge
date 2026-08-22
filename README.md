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
| Worker | `smith` | no | implements a bounded task in an owned worktree (codex `gpt-5.6-luna`) |
| Worker | `smith-grok`, `smith-grok-2` | no | same worker contract, pinned to grok `grok-4.6` |
| Worker | `smith-claude`, `smith-claude-2` | no | same worker contract, pinned to claude `claude-sonnet-5` |
| Forge-smith | `scout` | no | surveys the board and grooms bare cards into dispatch-ready specs |
| Reviewer | `assayer` | no | performs read-only, different-family review of an exact SHA |
| Verification gate | `verification-gate` | no | runs deterministic checks and records evidence for an exact SHA |

`CanonicalLaneRegistry` enforces unique lane **names**, not one lane per role:
several lanes may share a role, and the five worker lanes above do. Resolving a
lane *by role* therefore returns the first match, so address a specific lane by
name whenever more than one carries its role.

Roles are still a fixed set — `validateLaneLaunchConfig` accepts eleven, pinning
each to exactly one task shape — so a lane needs an existing role, not an
invented one. That is why the forge-repair lane is named `mender` and carries the
`recovery` role. Ten roles are occupied above; the unoccupied one is the
`assayer` *role*, which is distinct from the lane named `assayer` (that lane
carries the `reviewer` role).

Worker, forge-smith, and recovery lanes have checked-in Codex/Luna defaults,
but live routing may select another configured vendor harness and model. Pi is
not part of the supported fleet. Runtime model selection belongs to routing
policy; author and reviewer must not be pinned to the same model family for
R1–R3 work.

The `smith-grok*` and `smith-claude*` lanes exist to make that guarantee
schedulable rather than aspirational: each is pinned to one family, so the
fleet can always place an author and a reviewer in different families even
when one vendor is exhausted or cooled.

Spawn-ready prompt contracts are in `.herd/prompts/`.

## Requirements

- Go 1.25 or newer (go.mod requires 1.25.0)
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

`make build` installs the single executable at `bin/herd` and refreshes the
repository-root `herd` symlink used by cross-repository wrappers such as
Chainseer’s `bin/herdforge`. Both paths therefore always select the same
revision; use either path only after the build completes. The build first
creates and smoke-tests a temporary executable, then atomically renames it into
place. A failed compile or `herd --version` check leaves the previous binary
untouched. If every `herd` command suddenly exits with `SIGKILL`/137 and emits
no output, suspect a corrupted in-place rebuild—not OOM and not a parser bug.

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
herd review FAC-123 --pool
herd board-done FAC-123
```

`herd review <ref> --pool` is the signer-independent reviewer path. It pins
the candidate into a clean warm-pool slot, creates a relative symlink under
`.herd/review-surfaces/`, starts persistent OpenCode in a Herdr tab, and
delivers the review packet. The lease remains held until verdict ingest and
release; use `--no-launch` to prepare the surface without starting Herdr.

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

### The fence broker is required infrastructure

Every board status write is **fenced**: it carries an operation id and a fence
token, and it must be rejected if a newer fence has already advanced. Stock Kaneo
cannot enforce that — it has no native fence receiver and no operation dedupe —
so a fenced write needs something that does, or it fails closed.

That "something" is the fence broker. Without it a restored checkout has fence
state in `.herd/claim/fences.db` and no way to complete a close, and the first
symptom is a failed board write in the middle of a mutation. `herd preflight`
now reports this before work depends on it.

Pick exactly one of three postures:

**1. The coordinator hosts the broker in its own process.** Recommended for a
coordinator, and the intended contract rather than an implementation detail.

```zsh
export HERD_FENCE_COORDINATOR=1
herd board-done <ref>
```

Mint authority is the process address space: both credentials are generated in
the coordinator and never written to a file, an environment variable, or the
wire, so no same-UID worker can read them. Do not also run a standalone broker —
one live broker per claim volume is enforced by the claim-directory lock, and
hosting is refused when a broker URL is already configured.

**2. A standalone broker, with other processes as its clients.** Use this when
several processes must share one broker.

```zsh
herd fence-broker --claim-dir ./.herd/claim
export HERD_FENCE_BROKER_URL=<printed url>
export HERD_FENCE_BROKER_TOKEN=<printed worker token>
```

A worker receives only the worker token and can never mint a capability. The
mint token stays with whoever owns the broker process.

**3. An upstream board that natively enforces fence and operation dedupe.**

```zsh
export HERD_FENCE_ATOMIC_SERVER=1
```

Only set this against a board that genuinely enforces both. Stock Kaneo does
not, and asserting it there converts a fail-closed refusal into silent
double-application.

Hosting is never automatic. Taking the claim-directory lock would lock out every
other coordinator and any standalone broker on that volume, so it stays an
explicit choice. Workers, builders, and reviewers are refused outright: a worker
that hosted a broker would hold mint authority.

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
