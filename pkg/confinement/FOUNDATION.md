# Confinement foundation (FAC-190)

## One-sentence design rule

**The integrity store (profile + wrappers) must not live inside the worktree
write grant** — otherwise every confined agent can rewrite next-launch policy.

## What it enforces today

### Policy layer
- Authenticated capability: worktree + sentinel, AuthTuple, HMAC issuer proof.
- Absolute shared-root residual paths, traversal, symlink/case aliases denied.

### Integrity store (outside worktree)
- Session dir: `<shared>/.herd/confine-sessions/<task>/g<lease>/`
- Holds `profile.sb`, `bin/*` wrappers, `zdot/*` (ZDOTDIR), receipts
- Created by the coordinator **before** AgentStart; **not** in the worktree
  `file-write*` grant, so a confined agent cannot rewrite them
- Wrappers **re-hash the profile at every exec** (`shasum` check, exit 78 on mismatch)

### OS layer (Darwin `sandbox-exec`)
Grants:
- worktree tree
- worktree gitdir (`.git/worktrees/<name>`)
- common-dir **objects / info** (subpath)
- **literal** task branch ref + `.lock` and matching `logs/refs/...` only
  (never `filepath.Dir` / parent `refs/heads/task` subpath)
- common `refs/herd` + `logs/refs/herd` (namespaced anchors, not lane tips)
- `/tmp`, `/private/tmp`; agents get `TMPDIR=<worktree>/.herd/confine/tmp`
- `network*` / `process*` / `file-read*` for coding agents (write isolation is the FAC-190 surface)

Does **not** grant:
- common `packed-refs` / `packed-refs.lock`
- common `HEAD` / `HEAD.lock`
- sibling lane tips (`refs/heads/task/*` other than the literal task branch)
- hooks / config under common `.git`

Denies (deny-default):
- Shared-root residual (FAC-188 shape)
- Session integrity directory
- Sibling worktrees, hooks, cross-lane `refs/heads/*` (only the task branch)

Proves (live) for a `tee`/`git` child the coordinator spawns under the profile:
- Hermetic outside/sibling residual denials
- Shared-root residual denial
- In-worktree write
- `git hash-object -w` into common objects
- Hook write denial (linked topology)
- **Sibling branch ref write denial** (e.g. `refs/heads/task/fac-188-sibling`)
- **packed-refs / common HEAD write denial**
- **Confined rewrite of session profile fails**

Receipt field `wrapper_installed` means session wrappers exist and pass integrity
checks — not that herdr resolved the live agent through them (external CLI PATH).

### Shared root observation
- Read-only residual check: FAC-188 incident path must stay absent
- Does **not** digest coordinator `.herd` WAL/locks (stable under launch churn)

### Launch order
1. `PrepareOS` → session dir outside worktree + prove + freeze files
2. `TabCreate` with PATH/ZDOTDIR pointing at session
3. `BindAndProve` → MAC + re-prove + HMAC receipt
4. `AgentStart`

## Residuals (honest)

- `process*` / `mach*` / `file-read*` / `network*` are broad by necessity for
  coding agents; this is write-boundary confinement, not a confidential VM.
- If Herdr resolves agent kinds by absolute path ignoring PATH/ZDOTDIR, wrappers
  may not intercept (product dependency on PATH resolution).
- Nested `sandbox-exec` is kernel-refused for the running process; escape via
  rewriting integrity material is closed by placing it outside the write grant.
