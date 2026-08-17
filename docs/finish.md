# `herd finish`

`herd finish <ref> --landed-sha <sha>` is the coordinator's read-only,
post-merge completion gate. It verifies the exact landed commit, a sealed
completion receipt with an independent PASS review, required build and test
checks, a clean coordinator checkout, and removal of the task branch and
worktree. It exits non-zero for any missing or contradictory proof.

The command reports readiness for `herd approve`; it does not replace
`harvest-merge`, which performs integration, `herd verify`, which validates a
worker worktree before review, or `herd approve`, which is the separate
provider-backed board transition. `herd finish` never pushes, merges, or writes
board state.
