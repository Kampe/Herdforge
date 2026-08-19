# Runtime state

Herdforge creates mutable runtime state on demand. It is local operational
state, not repository configuration, and must remain ignored by Git.

- `.herd/herdforge.db` is the shared SQLite state database; its SQLite journal
  sidecars are runtime state too. Other subsystems keep their own runtime
  SQLite files under `.herd/` (for example `.herd/launch-claims.db`, which
  holds dispatch-time claim leases separately from `herdforge.db`'s leases
  table — check both when investigating a stuck claim). `.gitignore` covers
  every `.herd/*.db*` file with a wildcard rather than naming each one, so a
  new runtime database never needs a matching `.gitignore` update.
- SQLite constructors create a missing parent directory before applying their
  migrations, so a fresh clone does not require a seed database.
- `.herd/residual-artifact` is a confinement boundary marker: it must remain
  absent from a shared coordinator checkout. The marker is intentionally
  ticket-neutral; no production safety contract relies on a historical task
  filename.

Tracked `.herd` configuration, prompts, and reusable contracts remain source
artifacts. Historical task ledgers and runtime databases do not.
