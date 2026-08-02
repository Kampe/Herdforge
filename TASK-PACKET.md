# Task Packet: FAC-87

**Title**: Port herd-shared-checkout-lock: advisory mkdir-based serialization lock
**Priority**: no-priority
**Status**: in-progress
**Labels**: 

## Worktree

**Path**: `.herd/worktrees/fac-87`
**Branch**: `task/fac-87-port-herd-shared-checkout-lock-advisory-mkdir-based-serialization-lock`
**Role**: worker
**Agent**: opencode / litellm/lazer/deepseek-v4-flash
**Assigned Worktree**: .worktrees/worker

## Description

# Task Packet: FAC-87 — Port herd-shared-checkout-lock (advisory mkdir serialization lock) to Go

## Outcome (observable end state)

`herd lock` provides the same advisory, short-lived, mkdir-atomic mutual-exclusion primitive as `bin/herd-shared-checkout-lock`: modes `with`, `acquire`, `release`, `status`. `with` acquires before running a command and releases on every exit path (including panic/failure), exactly like the zsh trap. A crashed holder never wedges the fleet: a lock whose `holder` pid is dead, or whose directory is older than `HERD_SHARED_LOCK_MAX_AGE` (default 300s), is broken automatically before acquisition. Nested `with` calls are re-entrant via the `HERD_SHARED_LOCK_HELD` env marker so `dev-up -> apply-migrations` cannot self-deadlock. Tree-mutating git commands (pull/reset/rebase/checkout/stash/merge/switch) FAIL CLOSED (exit 3) against a dirty shared checkout unless `HERD_SHARED_DIRTY_OK=1`. Exit codes are contract: acquire 0 held / 1 timed out; with 0 ok (or child rc) / 2 usage / 3 dirty-refusal; status always 0.

## Source contract (bin/herd-shared-checkout-lock — quoted behavior that MUST survive)

WHY block, lines 1-7 (decision logic, quoted verbatim):

```
# herd-shared-checkout-lock: advisory, short-lived lock serializing MUTATIONS of
# the shared checkout ... A mid-repair in-place hotfix was raced out TWICE while the
# deployable was DOWN by concurrent `git pull --autostash` in that same checkout
# (2026-07-24, platform-ops). mkdir is atomic and portable, so no flock (absent
# on macOS). WITH a lock never blocks our raw git command.
```

Lock location & defaults, lines 21-24 (verbatim):

```
canonical="$(herd_canonical_root)"
lockdir="${HERD_SHARED_LOCK_DIR:-$canonical/.git/herd-shared-checkout.lock.d}"
holder="$lockdir/holder"
MAX_AGE="${HERD_SHARED_LOCK_MAX_AGE:-300}"
```

Stale breaking, lines 28-38 (the two and only two rules that auto-release):

```
_break_if_stale() {
  [[ -d "$lockdir" ]] || return 0
  local pid; pid="$(sed -n 's/^pid=//p' "$holder" 2>/dev/null | head -1 || true)"
  # dead holder -> stale
  if [[ -n "$pid" && "$pid" == <-> ]] && ! kill -0 "$pid" 2>/dev/null; then
    rm -rf "$lockdir" 2>/dev/null || true; return 0
  fi
  # too old -> stale
  if [[ -n "$(find "$lockdir" -maxdepth 0 -mmin "+$(( (MAX_AGE + 59) / 60 ))" 2>/dev/null)" ]]; then
    rm -rf "$lockdir" 2>/dev/null || true
  fi
}
```
> dead pid → remove; age ≥ MAX_AGE (minute-granularity mtime bound) → remove. Live pid + young dir → NOT stale.

Acquisition, lines 41-57 (verbatim): wait default 30, re-entrancy first:

```
[[ -n "${HERD_SHARED_LOCK_HELD:-}" ]] && return 0
while :; do
  _break_if_stale
  if mkdir "$lockdir" 2>/dev/null; then
    { print pid=$$; agent=${HERD_LANE:-${HERD_AGENT:-$(id -un 2>/dev/null || echo unknown)}}; reason=$reason; } > "$holder" ... || true
    return 0
  fi
  if (( waited >= wait )); then print -u2 -r -- "... locked by [$(_holder_str)], waited ${wait}s"; return 1; fi
  sleep 1; (( waited += 1 )) ...
done
```
> holder file carries `pid`, `agent` (env `HERD_LANE`/`HERD_AGENT`/user), `reason`. Loop polls mkdir every 1s until `waited >= wait` → 1, else 0. `_holder_str` prints the holder file with newlines collapsed to spaces, or `(unknown)`.

Wait/poll flag parse (lines 63-78): the leading mode then `--wait N` (default then to 30) and `--reason TEXT`, terminated by literal `--`.

Release (lines 60-67, advisory-only ownership note, quoted verbatim):

> `release` is the "I am done mutating the shared checkout" signal ... 'with' (the recommended form) acquires and releases in the SAME process, so it is naturally self-owned; bare acquire/release across two CLI invocations have different pids and cannot pid-match, so ownership is by convention (you acquire, you release), which is all an advisory lock promises.

Mode dispatch (lines 80-116):
- `status`: `LOCKED [<holder_str>]` or `unlocked`, always exit 0.
- `with`: requires a command after `--` (else usage, exit 2). Guard (lines 87-108): if `HERD_SHARED_DIRTY_OK != 1` AND first arg is `git` AND the joined arg list contains a space-wrapped `pull|reset|rebase|checkout|stash|merge|switch` → run `git -C $canonical status --porcelain`; if output, refuse on stderr, echo each dirty line indented two spaces, exit 3. CHA-544 rationale encoded in the comment (a plain WARNING was ignored and edits were destroyed twice). Then: if `HERD_SHARED_LOCK_HELD` set → run child without acquire/release (`$@; exit $?`); else `_acquire --wait --reason || exit 1`, `trap _release EXIT INT TERM`, run child with `HERD_SHARED_LOCK_HELD="$lockdir"`, release, remove trap, `exit $rc`. The child exit code is preserved as the command's exit code.
- `-h|--help|""` → print usage line, exit 0.
- unknown mode → usage to stderr, exit 2.

## Go design (real repo types)

New package `pkg/lock` (no existing package owns this; `pkg/worktree` creates/removes worktrees but nothing serializes shared-checkout mutation). Advisory dir lock in-process + command wrapper. Env interaction is the contract, so `HERD_SHARED_LOCK_HELD` must be set in the child's `cmd.Env` exactly like the zsh env marker.

1. `pkg/lock/lock.go`:

   ```go
   type DirLock struct {
       dir    string       // the lockdir
       holder string       // dir/holder
       maxAge time.Duration // HERD_SHARED_LOCK_MAX_AGE, default 300s
   }
   func NewDirLock(dir string) *DirLock
   func (l *DirLock) Acquire(ctx context.Context, wait time.Duration, reason string) error
   func (l *DirLock) Release()
   func (l *DirLock) Status() (held bool, holderStr string)
   // internal:
   func (l *DirLock) breakIfStale() (removed bool)
   func (l *DirLock) holderStr() string
   ```
   - `Acquire`: 1st guard — if `HERD_SHARED_LOCK_HELD` is set in `os.Environ()`, return immediately (re-entrant). Loop: break stale; `os.Mkdir(lockdir, 0755)` → on success write holder `pid=<os.Getpid()>\nagent=<HERD_LANE|HERD_AGENT|username>\nreason=<reason>\n` with `os.OpenFile(..., O_WRONLY|O_CREATE|O_TRUNC)`, return nil; else if elapsed ≥ wait return `fmt.Errorf("shared checkout locked by [%s], waited %ds", holderStr, waitedSeconds)`; else sleep 1s.
   - `breakIfStale`: dir missing → not stale. Read holder's `pid=` value (`regexp`/`bufio` first line). If pid parses as int and `syscall.Kill(pid, 0)` returns `ESRCH` → dead → `os.RemoveAll(lockdir)`. Else if `time.Since(mod)` of the dir `> time.Duration(MAX_AGE)*time.Second` → remove. (Go uses exact second precision; the zsh `find -mmin` bound is minute-granular — equivalence acceptable, both remove once it's been MAX_AGE.)
   - `Release()`: `os.RemoveAll(lockdir)`, `|| true` semantics.

2. `cmd/herd/main.go` — `case "lock": runLock()`: `flag.NewFlagSet("lock", flag.ExitOnError)`; first positional arg is `with|acquire|release|status`; `--wait` int (default 30), `--reason` string. For `with`, everything after literal `--` becomes the child command. Canonical root resolution: `HERD_CANONICAL_ROOT` if set and a directory, else the repo root (`.`), matching `herd_canonical_root`; lockdir default = `env HERD_SHARED_LOCK_DIR` else `<canonical>/.git/herd-shared-checkout.lock.d`; maxAge = `env HERD_SHARED_LOCK_MAX_AGE` (int seconds) else 300.
   - Dirty gate before acquire in `with`: first child arg must be `git`; then match the joined args against the pull/reset/rebase/checkout/stash/merge/switch token list (same space-delimited substring test zsh does); run `git -C <canonical> status --porcelain`; if any output lines → print the two-line refusal message then each line prefixed `"  "` to stderr, exit 3.
   - Reentrancy + trap semantics: `exec.CommandContext`, `cmd.Env = append(os.Environ(), "HERD_SHARED_LOCK_HELD="+lockdir)`; set a `defer lock.Release()` armed only after successful acquire (mirrors `trap _release EXIT INT TERM`); `os.Exit(rc)` where `rc` is the child's exit code (msg to stderr and `ExitError.ExitCode()` on error).

No `pkg/store`/`pkg/usage` involvement — this is a pure-filesystem primitive.

## Acceptance criteria (checkbox)

- [ ] `go test ./pkg/lock -count=1` green; `breakIfStale`/`Acquire`/`Status` fully table-tested with a temp dir as the lockdir (no real pid murder in tests — use a dead pid harvested in one subtest, plus an mtime-old dir).
- [ ] Two concurrent `Acquire` with the same dir → exactly one wins, the other returns an error after wait, holder file contains pid/agent/reason of the winner.
- [ ] Dead-holder recovery: holder with `pid=<dead-pid>` → next `Acquire`/`breakIfStale` removes the dir and wins.
- [ ] Age recovery: lockdir older than maxAge (default 300s) removed before acquire.
- [ ] Reentrancy: `HERD_SHARED_LOCK_HELD` set in env → `Acquire` returns immediately without touching the lockdir / creating `mk`.
- [ ] `with`: acquires, runs child, releases on success AND on child failure (exit propagated); `trap`-equivalent `defer Release` verified by a child that `os.Exit(1)`.
- [ ] Dirty gate: `with -- git pull` on a dirty canonical checkout refuses with the three-line CHA-544 message to score and exit 3; a clean checkout runs; `HERD_SHARED_DIRTY_OK=1` bypasses; non-git commands (compose build/test) never gate.
- [ ] Lockdir path overrides: `HERD_SHARED_LOCK_DIR`, `HERD_SHARED_LOCK_MAX_AGE`, `HERD_CANONICAL_ROOT` all honored; default canonical = repo root.
- [ ] `status` prints `LOCKED [...]`/`unlocked`, always exit 0; `-h`/no-arg prints usage, exit 0; unknown mode exit 2.
- [ ] Release is advisory: after `release`, a subsequent raw `mkdir` of the same path succeeds (no lingering removal).

## Test plan (table-driven, FIRST)

`pkg/lock/lock_test.go` — each case isolated in `t.TempDir()`; a dead-pid fixture is `runtime.GOOS`-portable via `pid=999999999` (never PID 1), and elapsed-maxAge cases use a short injected `maxAge` through `DirLock{maxAge:...}`.

| case | input | want |
| --- | --- | --- |
| fresh acquire | empty dir, wait 1s | nil, dir+holder exist |
| reentrant | env `HERD_SHARED_LOCK_HELD=dir` | nil, dir NOT recreated |
| timeout | existing lockdir (young) | error, waited message, no dir change |
| dead-holder stale | holder `pid=999999999` | dir removed, acquire nil |
| stale by age | dir mtime `now-maxAge-1s` | dir removed |
| young-holder keeps | dir mtime now | NOT stale |
| release removes | after acquire | dir gone; mkdir works |
| status locked/unlocked | present/absent dir | "LOCKED [pid 999999999 agent ...]"/"unlocked" |
| wait=0 | existing young lock | immediate error (0 polls) |

`cmd/herd` end-to-end: `herd lock with --wait 2 --reason "test" -- <true>` → exit 0, dir released; `<false>` → exit 1 and dir released; `-- git pull` with `FAKE unique dirty` fixture → exit 3 refusal; `HERD_SHARED_DIRTY_OK=1` → runs. Reuse the `execCommandContext`-style seam (`var execGit = exec.CommandContext`) so tests mock `git status --porcelain`.

## Workflow

1. Enter worktree: `cd .herd/worktrees/fac-87`
2. Inspect existing code and understand what needs to change
3. Write failing tests first (TDD)
4. Implement the minimal solution
5. Verify: `go test ./...` (or equivalent test command)
6. Commit with a conventional commit message
7. Signal completion by moving the card to 'in-progress' (review pipeline)

## Role Context

Role prompt from: `.herd/prompts/worker.md`
