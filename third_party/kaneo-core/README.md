# Kaneo core-read build input

Upstream: https://github.com/onreza/kaneo-cli
Pinned base: `f47a39dbae091c6842b5f46a788f083d21782cc8` (v0.11.1).
License: MIT, retained in LICENSE.

`core-read.patch` adds strict JSON task/project/label reads and local HTTP fixtures. It is a proposed dependency build input, not an installed binary. Review this patch with FAC-760; the upstream candidate is not a Herdforge ledger identity.

## Build and activate

Run `make kaneo-core` from a reviewed Herdforge checkout. This fetches the pinned upstream commit, applies the tracked patch, runs Rust tests and local HTTP fixtures, then publishes `bin/kaneo-core`. It never replaces global `kaneo`. Build failures publish nothing. Git, Cargo and Python3 are required; dependencies are locked by upstream Cargo.lock. Source is retained under `.herd/toolchains` for audit and coordinator cleanup.

Put this checkout's `bin` directory on PATH for the coordinator process and set `task_provider.core_task_reads: true` alongside `use_cli: true` in the intended repository configuration. The provider requires the separate `kaneo-core` executable to advertise `--core`; unsupported or failed capability checks refuse without a legacy retry. Other reads and all mutations continue to use ordinary `kaneo` and existing fences. Leave this setting false until the dependency is built and independently reviewed. Do not modify another repository's configuration without its owner's authority.

Rollback: set `core_task_reads: false`; the previous CLI transport remains intact. An older helper failing a read never triggers automatic transport fallback. An actual task timeout remains UNKNOWN, never an empty graph.

The core fixture proves four reads for a ref (search/task/project/labels) and rejects partial or inconsistent evidence. No optional activity/time/link/relation reads are made. Live latency and successful migration must be measured separately from fixture success.
