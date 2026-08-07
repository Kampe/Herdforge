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

## Residuals (honest)

- Wrapping the interactive Herdr agent argv itself (PATH seatbelt wrappers for
  every descendant of a live pane) still depends on Herdr process composition
  (FAC-172-class hosted isolation). Until that routes through `OSBackend.Wrap`,
  production **fails closed** without a successful pre-start OS proof rather than
  claiming ambient agent containment it cannot demonstrate.
- Bind mounts that preserve device numbers, and making an already-authorized
  write atomic, remain outside this package.
- Coordinator root/Git plumbing is a separate authority and is never delegated
  into a worker capability.
