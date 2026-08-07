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
Applied via a PATH wrapper named for **`Decision.Harness` (`pi`)** — production
`herdr agent start --kind pi` must resolve that name through the pane PATH.

Grants:
- worktree tree (`file-write*`)
- worktree gitdir (`.git/worktrees/<name>`, `file-write*`)
- common-dir **objects**: `file-write-create` + `file-write-data` only
  (not `file-write*`, which includes unlink and would allow `rm -rf objects`)
- common-dir **info** (`file-write*`)
- **literal** task branch ref + `.lock` and matching `logs/refs/...` only
  (never `filepath.Dir` / parent `refs/heads/task` subpath)
- common `refs/herd` + `logs/refs/herd` (namespaced anchors, not lane tips)
- **agent home state** (required for viable coding agents under the profile):
  `~/.claude`, `~/.codex`, `~/.local/share/opencode`, `~/.local/share/pi`,
  `~/.config`, `~/.cache`, and literal `~/.claude.json`
- `/tmp`, `/private/tmp`; agents get `TMPDIR=<worktree>/.herd/confine/tmp`
- `network*` / `process*` / `file-read*` for coding agents (write isolation is the FAC-190 surface)

Does **not** grant:
- `file-write-unlink` / `file-write*` on common objects (no destroy via rm)
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
- `git hash-object -w` into common objects (create still works)
- Profile text: objects are create+data only (never `file-write*`)
- Hook write denial (linked topology)
- **Sibling branch ref write denial** (e.g. `refs/heads/task/fac-188-sibling`)
- **packed-refs / common HEAD write denial**
- **Confined rewrite of session profile fails**

Object-store **unlink** denial is proved in disposable Darwin unit tests only —
never by running `rm`/`rm -rf` against the live shared objects dir on the
launch hot path.

Receipt field `wrapper_installed` means session wrappers named for the harness
exist and pass integrity checks. Production requires a `pi` wrapper. Absolute-
path resolution by herdr that bypasses PATH is still a residual.

### Shared root observation
- Read-only residual check: FAC-188 incident path must stay absent
- Does **not** digest coordinator `.herd` WAL/locks (stable under launch churn)

### Launch order
1. `PrepareOS` → session dir outside worktree + prove + freeze + **pi wrapper**
2. `TabCreate` with PATH/ZDOTDIR pointing at session
3. `PrepareToolChildLifecycle` → durable session generation N (written back)
4. `BindAndProve` → MAC over generation N + re-prove + HMAC receipt
5. `AgentStart` kind=`pi` under the same generation N

## Residuals (honest)

- `process*` / `mach*` / `file-read*` / `network*` are broad by necessity for
  coding agents; this is write-boundary confinement, not a confidential VM.
- If Herdr resolves agent kinds by absolute path ignoring PATH/ZDOTDIR, wrappers
  may not intercept (product dependency on PATH resolution).
- Nested `sandbox-exec` is kernel-refused for the running process; escape via
  rewriting integrity material is closed by placing it outside the write grant.
- Common `info/` still uses `file-write*` (narrower than objects; not cross-lane tips).
- Session dirs under `.herd/confine-sessions/` accumulate per lease generation
  (coordinator cleanup is out of band; gitignored when tracked from shared root).
- Agent home grants are broad directories under `$HOME` (viability > least privilege).
- `file-write-create` on `objects/` still allows planting files under
  `objects/pack/` (known residual; not closed by create+data alone).
- Loose-object mode `0444` is a POSIX protection, not a seatbelt property;
  overwrites of 0644 probe files under objects may still succeed.
