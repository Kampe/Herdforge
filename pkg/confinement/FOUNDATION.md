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
- common-dir **objects / branch-scoped refs / logs / info** (not whole `.git`, not hooks/config)
- `/tmp`, `/private/tmp`; agents get `TMPDIR=<worktree>/.herd/confine/tmp`
- `network*` / `process*` / `file-read*` for coding agents (write isolation is the FAC-190 surface)

Denies (deny-default):
- Shared-root residual (FAC-188 shape)
- Session integrity directory
- Sibling worktrees, hooks, cross-lane `refs/heads/*` (only the task branch)

Proves (live):
- Hermetic outside/sibling residual denials
- Shared-root residual denial
- In-worktree write
- `git hash-object -w` into common objects
- Hook write denial (linked topology)
- **Confined rewrite of session profile fails**

### Shared root observation
- Read-only: residual path must stay absent
- Digest is **stable** under coordinator `.herd` WAL/lock churn (does not list `.herd`)

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
