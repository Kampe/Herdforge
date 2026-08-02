# Herdforge Technical Manual and Agent Implementation Guide

- Version: 2.0-draft
- Runtime: compiled Go CLI at `cmd/herd`
- Normative workflow: [TARGET-WORKFLOW.md](TARGET-WORKFLOW.md)
- Current implementation delta: [AUDIT-2026-08-02.md](AUDIT-2026-08-02.md)

## Read this distinction first

Herdforge is designed to be a fail-closed, repo-agnostic software factory. Not every package or CLI command currently satisfies that design. Documentation in this guide uses:

- **must** for a binding invariant;
- **target** for behavior required before unattended operation;
- **current** for behavior verified in the present implementation.

Do not infer that a package name proves a capability is wired into the live control loop.

## Architecture boundary

Herdforge is the repository-local policy and state engine. Herdr is the agent execution plane.

Herdforge must decide what can advance, record why it advanced, bind work to immutable Git evidence, and recover incomplete transitions. Herdr should be used for session start/cwd, prompt delivery, provider availability, quota posture, attention, and session cleanup.

Task providers are adapters behind `provider.TaskProvider`. Provider-specific status strings, pagination, and error bodies must be normalized without weakening fail-closed behavior.

## Core implementation services

The target lifecycle is best expressed through small services rather than more logic in `cmd/herd/main.go` or `pkg/lifecycle/lifecycle.go`:

| Service | Responsibility |
| --- | --- |
| Eligibility planner | normalize tasks, enforce grooming/dependencies/role, sort deterministically |
| Claim service | cross-process compare-and-set, capacity token, lease generation, release/recovery |
| Event store/outbox | append-only task events and idempotent external mutations |
| Worktree service | fetch, immutable base, real branch/ref, cwd-bound session, durable candidate ref |
| Dispatch service | lean reference packet, verified delivery receipt, compensation on failure |
| Verification service | deterministic commands and non-vacuity evidence bound to SHA |
| Review service | risk tier, different-family selection, exact-SHA verdict ledger |
| Integration service | single-writer merge, post-merge verification, origin proof |
| Reconciler | compare board, leases, sessions, refs, worktrees, ledger, and `origin/main` |
| Scheduler | callback-driven advancement, settle watcher, periodic safety sweep, backpressure |

Packages that do not participate in one of these services should be explicitly experimental or deferred rather than implicitly production-ready.

## Task-provider contract

The existing interface is the narrow adapter seam:

```go
type TaskProvider interface {
    GetTask(ctx context.Context, id string) (*Task, error)
    ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error)
    ClaimTask(ctx context.Context, taskID string, role string) error
    UpdateStatus(ctx context.Context, taskID string, status string) error
    AddComment(ctx context.Context, taskID string, body string) error
}
```

The target claim semantics are stronger than this method signature. The claim service must add board revision/compare-and-set, a unique lease generation, idempotency key, and durable compensation/reconciliation around provider calls. A provider returning `200 OK` with an error body is a hard failure.

Canonical statuses are `to-do`, `in-progress`, `in-review`, and `done`. Adapters translate provider spellings at the boundary.

## Deterministic eligibility

Candidate order is:

```text
(priority DESC, ticket number ASC)
```

Sorting happens only after filtering. A task is not eligible unless acceptance criteria, role mapping, dependency state, risk information, and provider state are known. Unlabeled tasks must not be treated as matching every coding role.

The planner may summarize or propose missing data, but operator priority and product intent remain operator authority.

## Worktree and dispatch contract

For every implementation task:

1. fetch the configured default remote;
2. resolve an immutable `origin/<default-branch>` base;
3. atomically acquire a claim lease and capacity token;
4. create one owned task branch and worktree from that base;
5. create a durable ref protecting the work;
6. launch the agent with the worktree as the process cwd;
7. deliver a short reference packet and verify consumption;
8. compensate or enter Recovering if any step fails.

The recorded branch must be the branch Git actually created. A prompt telling the agent to `cd` is not an isolation boundary. Persistent role worktrees may be used for read-only control agents, not shared implementation state.

## Completion and verification contract

A worker completion callback contains the task ref, lease generation, branch, candidate SHA, clean-worktree state, and a concise summary. It is ignored if its lease is stale.

The verification gate runs configured commands against that exact SHA and emits a digestible artifact. It must:

- reject empty commands;
- preserve quoted arguments without unsafe shell reconstruction;
- propagate every non-zero exit;
- record command, duration, exit, and relevant environment policy;
- prove required negative assertions or mutations are non-vacuous;
- never repair source while acting as the verifier.

Only a passing receipt for the current candidate can enqueue review.

## Review and integration contract

R1–R3 changes require a reviewer from a different model family than the author. Backend, provider pool, model, and family are separate metadata. Unknown family or unavailable independent capacity blocks review; it is not permission to self-review.

Review is read-only and bound to candidate SHA plus patch ID. Valid verdicts are `PASS`, `FAIL`, and `BLOCKED`. Any new commit invalidates the verdict.

One integration owner holds the shared-checkout lock. Before merge it revalidates claim, revision, verification, review family, risk tier, and clean/rebase state. After merge it reruns configured gates, proves the candidate content on `origin/main`, updates the board idempotently, reads it back, and only then retires the exact session/worktree.

## Agent role contract

Role prompts live in `.herd/prompts/`. A prompt defines authority and prohibitions; it is not a substitute for technical enforcement.

- Orchestrator: coordinates only.
- Scout-planner: grooms and proposes eligibility only.
- Worker/smith: authors one task only.
- Verification gate: tests one candidate only.
- Reviewer: reviews one immutable candidate only.
- Review supervisor: routes evidence and capacity only.
- Harvest/integration owner: single-writer integration only.
- Recovery sentinel: evidence-based recovery only.

Do not assign the same agent or model family to incompatible roles on one change.

## Implementation workflow for coding agents

1. Confirm the assigned task ref, owned worktree, real branch, base SHA, and lease generation.
2. Run `make preflight` in the assigned worktree before editing.
3. Inspect the code graph before broad text/file exploration.
4. Write or update a test that fails for the intended regression; observe the failure.
5. Implement the smallest complete change.
6. Run targeted tests, then `make ci` unless the task packet specifies a stronger repository gate.
7. Confirm the worktree is clean after creating an atomic Conventional Commit containing the task ref.
8. Report the exact candidate SHA and verification evidence. Do not push, merge, review yourself, mutate board lifecycle, or clean sibling worktrees.

## Testing the orchestrator

Unit coverage alone is insufficient. The control plane requires concurrency and crash-point acceptance tests for:

- double claim attempts from two processes;
- failure after each external mutation;
- launch success with delivery failure;
- stale callback from a superseded lease;
- worker launched in the wrong cwd;
- candidate commit changed after verification or review;
- same-family review fallback;
- merge conflict and post-merge test failure;
- board success followed by failed readback;
- cleanup with dirty or unique work;
- lost callback recovered by reconciliation;
- full capacity backfill with review backpressure.

The operational readiness checklist in [TARGET-WORKFLOW.md](TARGET-WORKFLOW.md) is the release gate for unattended use.
