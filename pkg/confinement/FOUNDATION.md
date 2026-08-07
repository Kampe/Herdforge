# Confinement foundation (FAC-190)

This package is the production write-confinement surface for Herdforge task
worktrees. Round-3 redesign addresses the impossibility of a worktree-only
write sandbox for linked git worktrees and real coding agents.

## What it enforces today

### Policy layer
- Authenticated capability: worktree + sentinel device/inode identities, full
  AuthTuple, HMAC-SHA256 issuer proof.
- Absolute shared-root residual paths, traversal, symlink/case aliases, hardlinks
  denied; repo-relative writes under the bound worktree allowed.

### OS layer (Darwin `sandbox-exec`, first-match)
Grants (agent-viable — verified with live `git hash-object -w`):
- `file-write*` under the authenticated worktree
- `file-write*` under this worktree's **gitdir** (`.git/worktrees/<name>`)
- **Narrow common-dir grants only**: `objects/`, `refs/`, `logs/`, `info/`,
  plus top-level `HEAD`/`packed-refs`/lock files (not the whole `.git` tree)
- `file-write*` under `/tmp` and `/private/tmp` (agents also get
  `TMPDIR=<worktree>/.herd/confine/tmp`)
- `network*` for model API calls

Denies (deny-default + missing grants):
- **`common-dir/hooks`** and **`common-dir/config`** (no blanket common-dir
  allow — Darwin does not honor deny-before-allow for parent subpaths here)
- Shared-root residual paths (FAC-188 incident shape)
- Sibling worktrees and other paths outside the grants

### Shared root
- **Read-only observation** (`ObserveSharedRoot`) — never writes under the
  shared checkout. Detects FAC-188 residual presence and `.herd` listing drift.
- Worktree sentinel remains under the task worktree only.

### Launch order
1. `PrepareOS` — profile (with gitdir), prove denials **including linked gitdir
   write + shared residual deny**, install wrappers for provider **and** argv0,
   bind **profile content digest** into wrappers.
2. `TabCreate` with `PATH`, `ZDOTDIR` (worktree-local rc re-exports wrapper
   PATH first), and `HERD_CONFINEMENT_*` markers.
3. `BindAndProve` — MAC identity, re-prove, re-hash profile, verify wrappers
   still embed path+digest, **HMAC-sign receipt**.
4. `AgentStart` — wrappers force `sandbox-exec -f <profile>` when PATH resolves
   them.

### Receipts
- `ReceiptDigest` covers identity, OS fields, **profile digest**, wrapper names.
- `ReceiptMACHex` is HMAC-SHA256 over the digest with the confinement issuer
  secret (not forgeable from public fields alone).

## Residuals (honest)

- If Herdr resolves agent kinds by absolute path (ignoring PATH), wrappers do
  not intercept. Production still requires wrapper install + profile proof;
  live binary resolution depends on Herdr using PATH for kind executables.
- Login shells that ignore `ZDOTDIR` can still reorder PATH; wrappers that
  *do* run always re-enter `sandbox-exec` with the proved profile path.
- Profile file under the writable worktree can be rewritten after bind in a
  race with a concurrent writer; re-prove/re-hash narrows but does not close
  kernel-level races against a compromised worktree process.
- Non-Darwin hosts fail closed (`ErrOSUnavailable`).
