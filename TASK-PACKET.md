# Task Packet: FAC-104

**Title**: Port herd-worktrees: one-shot collision snapshot across worktrees
**Priority**: medium
**Status**: in-progress
**Labels**: 

## Worktree

**Path**: `.herd/worktrees/fac-104`
**Branch**: `task/fac-104-port-herd-worktrees-one-shot-collision-snapshot-across-worktrees`
**Role**: worker
**Agent**: opencode / litellm/lazer/deepseek-v4-flash
**Assigned Worktree**: .worktrees/worker

## Description

# Task Packet: FAC-104 — Port herd-worktrees (one-shot worktree collision snapshot) to Go

## Outcome (observable end state)

`herd worktrees` → `cmd/herd/worktrees.go` — pure-Go, zero-jq reimplementation of `bin/herd-worktrees`: prints a snapshot row per worktree of the shared checkout `{worktree, branch, head, locked, ahead, dirty, files}` plus a COLLISIONS section (any file touched — committed-ahead or dirty — by more than one worktree). Flags `--json` (machine array, exit 0) and `--files` (human table then a per-branch touched-file listing). Exit codes are contract: 0 success (json or human, collisions-none), 3 when collisions found (after printing them), 1 fatal (base missing, jq missing), 2 unknown arg. Patch-equivalence across `origin/main` is computed with `git cherry`, not rev-list, because this repo rebase-merges and reachability misreports already-merged branches as ahead (same reasoning as `bin/herd-unmerged`).

## Source contract (bin/herd-worktrees — quoted behavior that MUST survive)

Purpose + flag table, lines 1-27 (quoted verbatim):

```
# herd-worktrees , one-shot collision snapshot across every repo worktree:
# {worktree, branch, ahead-of-origin/main commits, dirty files, touched files}.
# Replaces the coordinator's manual per-worktree `git log` + `git status`
# loop when ranking claimable tickets (planner request 2026-07-21). A final
# COLLISIONS section lists any file touched (committed-ahead or dirty) by
# more than one worktree, so collision-checking is exact, not eyeballed.
#
#   bin/herd-worktrees            # human summary + collisions
#   bin/herd-worktrees --json     # machine-readable array
#   bin/herd-worktrees --files    # also list per-worktree touched files
```
`--json` `--files` `-h|--help` (prints lines 2-11, exit 0) `*) unknown arg` → stderr `herd-worktrees: unknown arg $1` exit 2.

Base resolution once (lines 31-36): `base="origin/main"` resolved from the repo root (`git rev-parse -q --verify origin/main`), every worktree shares refs. Not found → stderr + exit 1. `jq` dependency check at line 29 → Go must not require jq.

Worktree enumeration (lines 38-43): `git worktree list --porcelain`, `wt_paths` = lines starting `worktree ` with the path after the marker.

Per-worktree snapshot (lines 45-73) — the full computation:
- `[[ -d "$wt" ]] || continue` — a listed worktree whose dir vanished is still a collision signal, but is skipped (it has no files to read); behavior replicate: skip.
- `branch = git -C "$wt" rev-parse --abbrev-ref HEAD` else `"?"`
- `head = git -C "$wt" rev-parse --short HEAD` else `"?"`
- `locked = 0`; a worktree whose porcelain block contains the `locked` flag (awk pattern-match on `worktree <wt>` block) → 1.
- `ahead = git -C "$wt" cherry "$base" HEAD | grep -c ^+` — **patch-equivalence count** (line 53-55: "Patch-equivalence, not rev-list: this repo rebase-merges, so reachability misreports already-merged branches as ahead").
- `committed_files` when ahead>0: `git -C "$wt" diff --name-only "$base...HEAD"`, paths only, newline-separated (empty when no ahead commits).
- `dirty_files`: `git -C "$wt" status --porcelain | awk '{ $1=""; sub(/^ /,""); print }'` — strips the XY code column leaving the path.
- `dirty = count of dirty_files lines`.
- Row built with jq:
  ```
  {worktree:$wt, branch:$br, head:$sha, locked:($locked==1), ahead:$ahead, dirty:$dirty,
   files:((($cf | split("\n")) + ($df | split("\n"))) | map(select(length>0)) | unique)}
  ```
  i.e. files = unique sorted union of committed (ahead) + dirty/untracked paths; empty `$cf`/`$df` split on newline produces a `[""]` entry which `select(length>0)` drops.
- Rows accumulate as a JSON array in `rows`.

Output (lines 75-104):
- `--json`: `print -r -- "$rows" | jq '.'` → pretty JSON array, exit 0.
- Human (no --json): column `-t` table with columns `worktree\tbranch\tahead=N\tdirty=N` plus `\tlocked` suffix when locked (line 81).
- `--files`: blank line, then per row with files: `-- <branch>` then each file indented `   ` three spaces (line 85-87).
- COLLISIONS (human path only; `--json` early-exits before it): flatten all `files[]` into one `(path, branch)` list, `group_by(.f)`, `map(select(length > 1))` — only paths touched by more than one worktree — then per group line `"\(.[0].f)  <-  \([.[].b] | unique | join(", "))"` (lines 91-96 verbatim). Lines 98-104: if any → print `COLLISIONS:` + those lines then **exit 3**; else print `COLLISIONS: none` and fall through to exit 0.

## Go design (real repo types)

Reuses `pkg/worktree` (the `WorktreeManager` already shells `git worktree`). Note the Go port produces the same shape, but with a pure-Go walk of the bare worktree state — no `jq` dependency, no awk.

`cmd/herd/main.go` `case "worktrees":` new `runWorktrees()`:
- `flag.NewFlagSet("worktrees", flag.ContinueOnError)`; bools `json`, `files`; after flags, first non-flag arg is invalid (zsh has no positional).
- `Env` read via existing `firstEnv` (main.go:1744) for runtime values; `repo_root` via `firstEnv("HERD_ROOT", ...)` then cwd, consistent with `h..root()`.
- Base: run `git -C repoRoot rev-parse -q --verify origin/main`; if NOT ok → stderr `herd-worktrees: origin/main not found`; exit 1.
- Enumerate: `git -C repoRoot worktree list --porcelain`, split lines, collect ones that start with `worktree `, and from the porcelain parse the leading path (and capture the `locked` flag if present in the same block). The `locked` parse is a pure walk of the porcelain blocks, one `worktree <path> HEAD <sha> [branch <b>|detached] [locked ...]` block each — detect `\nlocked` inside a block.
- For each worktree directory (skip missing): run via `execCommandContext` (the seam already used by `pkg/worktree`):
  - `git -C wt rev-parse --abbrev-ref HEAD` → or `?`
  - `git -C wt rev-parse --short HEAD` → or `?`
  - `git -C wt cherry origin/main HEAD` filtered by lines starting `+` → count = ahead
  - when ahead>0: `git -C wt diff --name-only origin/main...HEAD` → committed files
  - `git -C wt status --porcelain` → strip first column, keep paths → dirty files
  - `git -C wt status --porcelain` count → dirty
  - build row, append to `[]Row` slice.
- JSON marshal: `[]Row` with fields Worktree, Branch, Head, Locked(bool), Ahead(int), Dirty(int), Files []string (unique, sorted). `--json`: `json.MarshalIndent` to stdout, exit 0.
- Human: tab-writer + `text/tabwriter` aligned to `column -t -s $'\t'`, `ahead=N dirty=M` then `\tlocked` suffix.
- `--files`: `-- <branch>` then `   ` + each file, mirroring the script.
- Collisions: build map path→[]branches; keys with ≥2 distinct branches → lines `"\(f)  <-  \(join(", ", unique-branches))`; non-empty → `COLLISIONS:` + lines, **exit 3**; empty → `COLLISIONS: none`, exit 0.

`pkg/worktree` gains (small additive refactor) a `SnapshotRows`-style method or the commands calls `git` directly with the seam; keep `Row`/`Branch`/`Head`/`Locked`/`Ahead`/`Dirty`/`Files` types in `cmd/herd/worktrees.go` (single-use, not shared library surface).

## Acceptance criteria (checkbox)

- [ ] `go test ./pkg/worktree/... ./cmd/herd -count=1` green.
- [ ] Human output byte-identical to the zsh `column -t` rendering for a fixture repo (same columns, ordering, spacing, `locked` suffix).
- [ ] `--json` outputs a valid JSON array; `worktree`, `branch`, `head` short-sha, `locked` bool, `ahead`, `dirty`, `files` all present.
- [ ] `--files` adds the per-branch `--` + 3-space-indented touch lines after the table.
- [ ] COLLISIONS: a file touched by 2+ worktrees listed under `COLLISIONS:` with unique branch set, exit 3; none → `COLLISIONS: none` exit 0.
- [ ] Missing-origin/main → stderr `herd-worktrees: origin/main not found` + exit 1.
- [ ] `--help` prints the header usage and exits 0.
- [ ] Unknown arg → stderr `unknown arg X` + exit 2.
- [ ] `--json --files` → json only (files section omitted like zsh's `if (( json )); then exit 0` early-out).
- [ ] No jq in any output path (Go built-in JSON only).
- [ ] Ahead uses `git cherry` (patch-equivalence), NOT rev-list count (rebased branch fixture must report ahead 0).

## Test plan (table-driven, FIRST)

For the git operations use a fixture built in `t.TempDir()`: initialize a bare repo with origin/main at one commit, add two worktrees (one clean on branch A with 1 ahead commit touching f1, one dirty on branch B touching f1 + untracked f2), then assert the Go rows. To keep FIRST (fast) use the `execCommandContext` seam hoisted from `pkg/worktree` into the test to fake `git cherry` outputs in unit tests, and one integration test against the real fixture.

| case | setup | want |
| --- | --- | --- |
| empty main | no worktrees | `[]` row, 0 |
| one clean wt | branch A, head short-sha, ahead 0 | row branch=A head=… ahead=0 dirty=0 locked=false files=[] |
| ahead commit | branch A +1 patch-equivalent commit | ahead=1 files=[f1] |
| rebased branch | branch A rebased onto origin/main, same commit | ahead=0 (cherry counts not reachable-ahead) |
| dirty wt | branch B dirty f2 + untracked f3 | dirty=2 files=[f2,f3] |
| locked wt | porcelain block containing `locked` | locked=true |
| shared file | A touches f1, B touches f1 | COLLISIONS: f1 <- A, B; exit 3 |
| no collisions | A touches f1, B touches f2 | `COLLISIONS: none` exit 0 |
| missing wt dir | listed path absent | skipped, not an error |
| missing origin/main | bare repo w/o ref | exit 1 |
| unknown arg | `--nope` | exit 2 |
| --help | any | exit 0 |
| --json + --files | json branch | json only, exit 0 |
| porcelain locking flag | w/ `locked` in block | Locked true (pure porcelain walk) |
| tabwriter human | alignment | `column -t`-equivalent columns |

## Workflow

1. Enter worktree: `cd .herd/worktrees/fac-104`
2. Inspect existing code and understand what needs to change
3. Write failing tests first (TDD)
4. Implement the minimal solution
5. Verify: `go test ./...` (or equivalent test command)
6. Commit with a conventional commit message
7. Signal completion by moving the card to 'in-progress' (review pipeline)

## Role Context

Role prompt from: `.herd/prompts/worker.md`
