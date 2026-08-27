# Second host: WSL2 review pool

Adds a Windows machine to the fleet as a **review-only** worker pool. Every
constraint below was verified against this repository, and the things that do not
work are named as such rather than left for the next person to discover.

## What works and what does not

`herd` **does not build for native Windows.** `GOOS=windows go build ./cmd/herd`
fails in more than ten packages on Unix-only primitives:

- `syscall.Flock` / `LOCK_EX` — the locks protecting the review ledger, posture
  authority, mail and the fence broker
- `syscall.Kill` / `Getpgid` / `Getpgrp` / `Setpgid` — process-group ownership of
  agent lifecycles

Those are load-bearing, not incidental. Porting them means reimplementing the
locking and process-ownership model on a second platform.

`herd` **does build for WSL2 unchanged**: `GOOS=linux GOARCH=amd64` produces a
working binary with zero source changes. herdr already runs in WSL2. So the
second host runs the fleet inside WSL2, with its own checkout, and behaves
exactly as the primary does.

## Division of work

**WSL host: reviews only.** Reviews are the ideal remote workload — read-only
against a SHA-pinned worktree, hermetic, no shared dev stack, no compose, no OOM
exposure — and their bottleneck is quantity, which is exactly what a second host
buys.

**Primary host keeps everything stateful:** the shared `chainseer-e0` stack,
`bin/ci-local`, builders that need blackbox, and — non-negotiably — sole
ownership of the review ledger and pool state.

### One ledger writer, ever

`.herd/review-ledger.jsonl`, `.herd/pool/pool.json` and the container store are
`Flock`-protected **within a host**. Flock does not coordinate across hosts.

Two hosts running `review-ingest` against one ledger will corrupt it in a way
that is very hard to detect afterwards. The failure class this fleet has already
paid for repeatedly is *review work silently lost between the reviewer and the
ledger* (FAC-583, FAC-584, FAC-590, FAC-597). Do not add a distributed-write
variant of it.

## Verdicts travel as commits

Git is the transport. The WSL host never writes the primary's `.herd/`.

1. WSL reviewer writes its verdict to the absolute path its packet names
   (FAC-597 made the packet state that path explicitly).
2. WSL host commits verdict artifacts to a per-host branch: `verdicts/<host-id>`.
3. Primary pulls that branch and runs `herd review-ingest` on the files.

Every artifact has a distinct filename, so the branch is append-only and
conflict-free by construction. This reuses the ingest contract that already
exists — an explicit file list — rather than inventing a merge story.

## Per-host configuration

Two runtime overrides make this work without touching tracked config. Both
already exist; nothing here needs a code change.

`HERD_CONFIG_PATH` selects a config profile at runtime. It exists precisely so a
host does not have to edit the repository default. The chainseer `.herd/herd.yaml`
pins `herdr_workspace`, which is host-specific — editing it in place would
conflict on every pull.

`HERD_WORKSPACE` wins unconditionally for workspace resolution, with no herdr
call. A mismatch against the registered workspace is refused outright
(`refusing cross-workspace mutation`), which is the guard doing its job.

```bash
# ~/.herd/env.sh on the WSL host, sourced by its shell profile
export HERD_CONFIG_PATH="$HOME/chainseer/.herd/herd.wsl.yaml"
export HERD_WORKSPACE="<the herdr workspace id on THIS host>"
export GOCACHE=/tmp/shared-gocache
unset GOROOT   # a persisted Go env pins a stale GOROOT and outranks PATH
```

`herd.wsl.yaml` is a copy of `herd.yaml` with `herdr_workspace` set to this
host's id.

### Container protection list

`herd containers reap` protects compose projects by explicit name, defaulting to
`chainseer-e0`. The WSL host has its own stack names, so pass its own list:

```bash
herd containers reap --protect "<this host's long-lived compose projects>"
```

Wrong in the safe direction (an unfamiliar stack is kept forever) is harmless.
Wrong the other way reaps a stack other lanes depend on, which is an outage
rather than a cleanup. The dry run is the default; use it first.

## Bootstrap

```bash
# in WSL2
git clone <chainseer remote> ~/chainseer
git clone <herdforge remote> ~/Herdforge

cd ~/Herdforge
unset GOROOT
GOCACHE=/tmp/shared-gocache go build -o ./herd ./cmd/herd   # native linux build
./herd --version

cd ~/chainseer
cp .herd/herd.yaml .herd/herd.wsl.yaml
# set herdr_workspace in herd.wsl.yaml to this host's workspace id
source ~/.herd/env.sh

./bin/herdforge preflight        # must pass before dispatching anything
./bin/herdforge quota            # confirm this host sees its own provider quota
./bin/herdforge containers reap  # dry run; confirm it proposes nothing surprising
```

`preflight` is the gate on whether this checkout may dispatch at all. Do not skip
it because the primary passes; it checks *this* host.

## Capacity before remote worktree preparation (FAC-584)

`herd review --pool` refuses on the review host before candidate resolution or
worktree preparation when Herdr is down, the agent census is unreadable, live
reviewer slots are full, or memory headroom cannot admit another reviewer. The
same check is the contract for any remote launcher (including
`herd-review-remote`):

```text
ssh <review-host> 'cd <repo> && herd capacity --json --claim'
```

A non-zero exit or `"admit": false` means do not create a remote worktree and do
not launch a reviewer. `--claim` holds a host-local admission lease so concurrent
callers cannot all pass the same census. The JSON document carries
`schema_version`, `observed_at`, Herdr state, memory, process census, limits,
`available_slots`, `admit`, and a durable `reason`.

## Why review-only, and not builders

A builder needs the live stack for blackbox work, and the stack is tied to one
Docker VM per host. Standing up a second shared stack doubles the container-leak
surface that FAC-598 exists to contain, and buys nothing that extra review
throughput does not already buy. Revisit only when review capacity stops being
the constraint.
