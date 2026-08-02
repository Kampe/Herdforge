# Critical reliability wave coordinator

## Objective

Move Herdforge from a collection of partially connected primitives to a reliable factory foundation by completing FAC-119 through FAC-122 with isolated workers, independent review, and integration evidence.

## Source of truth

- `AGENTS.md` and the repository engineering invariants.
- `docs/architecture/AUDIT-SECOND-PASS-2026-08-02.md` for confirmed failure modes.
- The live, full Kaneo descriptions for FAC-119, FAC-120, FAC-121, and FAC-122.
- Repository code and tests override stale board claims about what has shipped.

## Constraints

- Do not write in the shared checkout. Create every worker from the current fetched `origin/main` in a separate task worktree and branch.
- Do not duplicate FAC-64, FAC-68, FAC-81, or FAC-84; they are already active.
- A worker must atomically claim its own card and confirm the `forge-smith` role label before editing.
- Run `make preflight` before editing. Commit each coherent increment with a Conventional Commit message. Never add attribution trailers.
- Keep the first parallel slices package-local. Serialize shared `cmd/herd`, store-schema, provider-interface, and integration wiring.
- Treat all four tickets as R3. A different model family must review the exact candidate SHA before integration.
- A commit subject or worker statement is not completion evidence. Record commands, exits, candidate SHA, reviewer family/verdict, merge SHA, and provider readback.
- If the current CLI cannot prove a gate, stop at an explicit blocked state; do not mark the card Done.

## Wave plan

Launch up to four workers after checking live claims and file scopes:

1. FAC-119 owns the canonical lifecycle/event/outbox contracts and their persistence schema. It publishes the interfaces the other slices consume.
2. FAC-120 owns lease/fencing behavior in `pkg/claim` and claim-focused tests. Coordinate any persistence-schema change with FAC-119; do not edit the same schema file concurrently.
3. FAC-121 owns dispatch/worktree/Herdr cwd enforcement and crash-point tests. It consumes lifecycle and lease interfaces and defers shared CLI wiring until integration.
4. FAC-122 owns exact-SHA verification receipts and real mutation execution in `pkg/verifier`. It defers shared CLI/review-admission wiring until integration.

FAC-132, FAC-139, and FAC-138 are the next wave, in that order where dependencies require it. Do not start them concurrently with overlapping prerequisite work merely to increase utilization.

## Required launch report

For each worker report:

- Kaneo ref and successful claim/readback.
- agent name, harness/model family, worktree, and branch.
- exclusive initial write scope and known collision boundary.
- preflight result and first intended commit.

## Completion

The wave is complete only after exact-SHA review and serial integration of all four compatible slices, `make ci` passes from the integrated revision, Kaneo readback matches evidence, and the next-wave dependency order is updated. Delegation alone is not completion.
