# Confinement foundation (FAC-190)

This package is the production write-confinement surface for Herdforge task
worktrees.

## What it enforces today

- **Authenticated capability**: a capability binds canonical worktree + sentinel
  device/inode identities, the full repository/task/lease/lane/session/Herdr/
  process/argv/policy/allowed-roots tuple, and an HMAC-SHA256 issuer proof.
- **Policy path checks**: absolute shared-root paths, `..` traversal, symlink and
  case aliases, hardlinks, different devices, and sentinel mutation are denied;
  repo-relative writes under the bound worktree are allowed.
- **Worktree sentinel**: every created/reattached task worktree installs
  `.herd/worktree-sentinel`. The shared checkout installs
  `.herd/shared-root-sentinel` and revalidates it around launch.
- **OS write-denial proof (Darwin)**: `sandbox-exec` hermetic probes must fail to
  create the FAC-188 incident-shaped absolute shared-root file and sibling
  writes, while an in-worktree write succeeds, before production dispatch starts
  an agent.
- **Durable receipts**: `BindAndProve` appends JSONL evidence under the worktree
  when a receipt directory is configured.

## Production launch order (FAC-190)

1. `PrepareOS` — write first-match-safe profile (worktree write only), prove
   hermetic denials with `/usr/bin/tee` under `sandbox-exec` (no shell, no
   shared-root mutation), install PATH-first agent wrapper under
   `.herd/confine/bin/<kind>`.
2. `TabCreate` with `PATH=<wrapper-bin>:$PATH` so herdr kind resolution hits the
   wrapper.
3. `BindAndProve` — MAC-bind tab/pane/lease identity, re-prove with the same
   profile, require `AgentWrapped && OSProved` before `AgentStart`.

## Residuals (honest)

- Herdr must resolve the agent kind via PATH for the wrapper to exec. If a
  future herdr release hardcodes absolute agent binaries, production will still
  fail closed on missing wrap install but the live process may bypass the
  wrapper — that is a Herdr contract change, not a silent policy skip.
- Bind mounts that preserve device numbers, and making an already-authorized
  write atomic, remain outside this package.
- Coordinator root/Git plumbing is a separate authority and is never delegated
  into a worker capability.
- `sandbox-exec` is deprecated by Apple; when unavailable `RequireOS` fails
  closed (no policy-only production admission).
