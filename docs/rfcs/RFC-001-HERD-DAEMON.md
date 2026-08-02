# RFC-001: Herdforge Durable Daemon and CLI

- Status: Draft; direction accepted, end-to-end implementation incomplete
- Target repository: Herdforge
- Normative detail: [Target Workflow](../architecture/TARGET-WORKFLOW.md)

## Summary

Herdforge is a compiled, repo-agnostic control plane that advances work from a task provider through isolated implementation, deterministic verification, independent review, serialized integration, and reconciled completion. Herdr supplies agent-session execution, delivery, provider/quota visibility, and process lifecycle.

The daemon must be a durable event and reconciliation engine. It is not merely a timer that moves cards between columns.

## Goals

- Harvest every groomed, dependency-clear task whenever compatible capacity is safely available.
- Prevent duplicate claims and stale callbacks across processes and restarts.
- Bind dispatch, verification, review, and integration to an immutable candidate SHA.
- Require different-model-family review for R1–R3 changes.
- Recover every partial transition idempotently after crash or external failure.
- Support multiple task providers without embedding one provider’s CLI semantics in the engine.
- Keep the default branch and shared checkout under one serialized integration owner.

## Non-goals

- Reimplementing Herdr terminal, quota, or execution-surface mechanics.
- Porting every Chainseer script byte for byte.
- Treating a Kanban column as the authoritative transaction log.
- Keeping builders busy by bypassing grooming, review pressure, resource gates, or isolation.

## Architecture

The daemon coordinates these durable services:

1. provider normalization and eligibility planning;
2. atomic claim leases and capacity tokens;
3. append-only events and transactional outbox;
4. immutable-base task worktrees and cwd-bound Herdr dispatch;
5. completion callbacks and deterministic verification;
6. risk classification and exact-SHA independent review;
7. single-writer integration, post-merge gates, and origin proof;
8. board reconciliation and exact resource cleanup;
9. settle-driven recovery plus periodic safety reconciliation.

Each task transition carries an idempotency key and expected prior state. A provider error, error-bearing success body, failed receipt, unknown family, stale revision, or mismatched readback stops advancement.

## Event model

Events are append-only and sequenced per task:

```text
task.discovered
task.eligible
claim.acquired
dispatch.started
dispatch.consumed
candidate.reported
verification.passed | verification.failed
review.requested
review.passed | review.failed | review.blocked
integration.started
integration.passed | integration.failed
board.reconciled
resources.cleaned
recovery.required | recovery.completed
```

Every event includes repository, task ref, lease generation, actor role, timestamp, and the revision/evidence identities relevant to that stage. Consumers ignore duplicates and reject stale lease generations.

## Scheduler

The primary trigger is an event: completion, failure, capacity release, quota change, board change, or integration. A settle-driven watcher batches noisy changes. A slower periodic sweep reconciles lost events and external drift.

The scheduler fills verifier and reviewer capacity before creating excess builder pressure, honors resource/budget/review caps, then backfills from eligible tasks sorted by `(priority DESC, ticket number ASC)`.

## CLI direction

The CLI is an operator and automation interface over the same state machine. Commands such as `pulse`, `dispatch`, `verify`, `review`, `harvest`, `board-done`, and `daemon` must call shared transition services rather than implement separate lifecycle semantics.

Read-only commands such as `status`, `next`, `attention`, `worktrees`, `unmerged`, `throughput`, and `board-sync` must propagate unknown/error posture accurately.

The checked-out binary’s `herd --help` remains authoritative while command names stabilize.

## Required guarantees

1. Fail-closed exits and provider responses.
2. Non-vacuous verification and mutation checks.
3. Repo-relative configuration and cwd-enforced task isolation.
4. Deterministic selection after role/dependency eligibility filtering.
5. Cross-process claim fencing and crash recovery.
6. Different-family exact-SHA review for R1–R3.
7. One integration writer and post-merge verification.
8. `origin/main` proof before board `done`.
9. No cleanup of dirty or unique work.
10. Callback-driven backfill with reconciliation fallback.

## Acceptance

This RFC is implemented only after concurrency and crash-point tests prove every guarantee, including double-claim prevention, wrong-cwd rejection, stale-review rejection, recovery after each partial mutation, and preservation of unique work during cleanup.
