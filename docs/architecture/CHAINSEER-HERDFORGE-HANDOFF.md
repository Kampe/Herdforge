# Chainseer → Herdforge handoff

This document records the current boundary between Chainseer’s shell control
plane and Herdforge’s Go implementation. It is operational documentation for
the parity work; Chainseer-specific policy remains outside Herdforge core.

## Parity source of truth

Parity measurements must invoke the built binary at `bin/herd`, not the stale
repository-root `herd` file. The checked-in parity manifest is
[`chainseer-bin-parity.json`](chainseer-bin-parity.json), and the audit is
deterministic and read-only:

```sh
env -u GOROOT go run ./scripts/binparity \
  -source ../chainseer/bin \
  -manifest docs/architecture/chainseer-bin-parity.json
```

The manifest currently covers 124 executable files. Chainseer’s live command
invocation measurement remains a separate compatibility figure: 79
non-library scripts, 55 implemented by invocation, and 24 unknown. Name
matches are not proof of behavioral parity; use the manifest and differential
tests before retiring a shell command.

## Candidate and review fencing

Review discovery is keyed by `(task_ref, candidate_sha)`. When callback,
ledger, inbox, and worktree evidence disagree, the authoritative candidate is
the newest observed evidence by timestamp. Source rank is only a deterministic
tie-break, never a reason to prefer an older SHA. Evidence and verdicts stay
attached to their exact SHA, so an older blocked or passing record cannot leak
onto a newer candidate.

This behavior is covered by FAC-312 regression tests in
`pkg/candidateindex/index_test.go`, including repeated builds that exercise
map-order independence.

## Large-board graph snapshots

FAC-310 keeps relation snapshots provider-neutral and fail-closed. Large
boards use bounded batches and capped concurrency, propagate cancellation, and
retain dual-end relation agreement. Boards with at least 64 tasks receive the
existing two-minute graph deadline extension; ordinary list operations keep
their normal deadline. Unknown, inconsistent, or incomplete relation evidence
is an error rather than an empty graph.

## Handoff checklist

Before retiring a Chainseer script or changing a parity disposition:

1. Pin the measured source revision and use `bin/herd` explicitly.
2. Run the parity audit and record its output.
3. Compare behavior on the same input, not only command names.
4. Keep Chainseer product policy and exemptions in the manifest rationale.
5. Run candidate/review tests when changing dispatch or review discovery.
6. Remove only the exact completed Herdr worktree and tab; leave unrelated
   workspaces untouched.
