# Task Packet: FAC-62

**Title**: Port herd-board-sync: bi-directional Kaneo-vs-git reconciliation (FAC-61)
**Priority**: no-priority
**Status**: in-progress
**Labels**: 

## Worktree

**Path**: `.herd/worktrees/fac-62`
**Branch**: `task/fac-62-port-herd-board-sync-bi-directional-kaneo-vs-git-reconciliation-fac-61`
**Role**: worker
**Agent**: opencode / litellm/lazer/deepseek-v4-flash
**Assigned Worktree**: .worktrees/worker

## Description

# Task Packet: FAC-62 — Port herd-board-sync (bi-directional Kaneo↔git reconciliation) to Go

## Outcome (observable end state)

`herd board-sync` runs against the real provider and reports every drift between **to-do / in-progress / in-review** tickets and git reality (merged commits, live worktree branches, lanes holding unpushed work). A pure matcher in `pkg/sync` decides "is a ref SHIPPED on origin/main" with **subject-only, mention-preceded, and createdAt date-gate** semantics, ported verbatim so tests hit the real logic (`--ship-check` entrypoint becomes a `--selftest` flag as in `runBoardDone`). Real reconciliation replaces the `ReconcileBoard` stub in `pkg/sync`; `BoardDone`/`MergeEvidence`/`NormalizeRef` stay untouched for `herd board-done`. **Report-only**: the Go command never writes status to the provider; findings carry exact remediation text. Exit codes and flags mirror `bin/herd-board-sync` exactly.

## Source contract (bin/herd-board-sync, quoted behavior that MUST survive)

Flags / env / exit codes (lines 20-55, 225-235, 344):

```
> bin/herd-board-sync [--json | --daemon [--interval SECONDS] | --ensure-daemon | --cache-check]
>   --daemon       refresh the durable cache until stopped
>   --ensure-daemon start one daemon if the cache owner is absent
>   --cache-check  read the cache; exit 4 when missing or stale
```
| Flag / env | Semantics | Go equivalent |
| --- | --- | --- |
| `--json` | `{"drift":N,"findings":[{...}]}` | same object |
| `--daemon [--interval SECONDS]` | loop refresh of durable cache until stopped | daemon loop in CLI |
| `--ensure-daemon` | start one daemon if pid-file owner absent | pid-file logic in CLI |
| `--cache-check` | read cache; exit 4 missing/stale, 3 drift cached, 0 clean | jq→Go JSON |
| `-h/--help` | exit 0 | flag help |
| any unknown arg | `print ... "unknown arg" >&2; exit 2` | exit 2 |
| `interval` not a positive integer | `"interval must be a positive integer"` exit 2 | flag Int + validate |
| kaneo CLI / jq missing | `"kaneo CLI not on PATH"` / `"jq required"` >&2 exit 1 | exit 1 |
| kaneo returns non-JSON | `"kaneo task list returned non-JSON"` >&2 exit 1 | exit 1 |
| `HERD_KANEO_PROJECT` | project id, default `ypsjln1upv5rbxapxbr4mluz` (bin line 228) | config |
| `HERD_BOARD_SYNC_INTERVAL` | default `120` | env default 120 |
| `HERD_STATE_DIR`/.port cache | cache file `board-sync.json`, pid file, log file | state-dir layout |

Ticket filter and classification MUST match the zsh exactly (lines 271-277 and 304-331):

```
> select((.status // "") | test("^(to-do|in-progress|in-review)$")) |
> select((.title // "") | test("standing epic"; "i") | not) |
```
> "Tickets titled 'standing epic' are intentionally long-running and skipped."

```
if (( active )); then          # being worked, board is honest
elif (( merged )); then        # SHIPPED?  -> "verify, then: kaneo task status $tid done"
elif (( work_in_flight )); then # UNKNOWN   -> "cannot prove dead, do NOT flip to to-do"
else                            # STALE     -> "dead claim"
```
```
to-do: if active -> "BOARD-LAG $ref (to-do) has a live worktree branch -> kaneo task status $tid in-progress"
```

`active` is `branch names` OR unpushed commit subjects OR any work in flight (lines 291-298):
```
> branches: git worktree list --porcelain → '^branch refs/heads/' lowercased
> (ref names matching (${lref}|${nref})([^0-9](_|$)) in $branches) => active
> (ref names matching in "$local_refs") => active   # subjects+bodies of commits ahead of origin/main in each worktree
> any worktree has `git status --porcelain` non-empty => work_in_flight=1
```

SHIPPED matcher (lines 78-102) is quoted in full in the acceptance criteria; here is the inescapable shape:
```
> merged_log = git log origin/main -500 --pretty='%ct%x09%s'  (lowercased)
> a ref counts as SHIPPED only when a merged commit satisfies ALL of:
>   - the ref is in the commit SUBJECT (merged_log is %s, so body-only mentions never count);
>   - that subject occurrence is NOT mention-preceded ("... after CHA-476", "R2 follow-up on CHA-268");
>   - the commit is NOT OLDER than the ticket's createdAt (kills ref-reuse-across-rollback mechanically).
> created=0 disables only the date gate.
```

## Go design (real repo types)

Everything lands in `pkg/sync` + `cmd/herd/main.go`, extending the existing harness.

1. `pkg/sync/boardsync.go` — the real reconciliation:
   - Keep `BoardSyncer{Provider: provider.TaskProvider}` and `NewBoardSyncer`.
   - **Replace** the stub `ReconcileBoard(ctx, projectID string) (*SyncReport, error)` with the real one — a FIXED signature that includes the repo:
     ```go
     // ReconcileBoard reconcilis the board against git reality, returning drift
     // findings. Porting bin/herd-board-sync. repoDir is where git operations
     // run (runBoardSync passes "."); report-only: it never writes status.
     func (b *BoardSyncer) ReconcileBoard(ctx context.Context, projectID, repoDir string) (*BoardDrift, error)
     ```
   - `type BoardDrift struct { Drift int; Findings []BoardFinding }`
   - `type BoardFinding struct { Kind, Ref, TaskID, Status, Title, Action string }`
     with `Kind` `SHIPPED`, `UNKNOWN`, `STALE`, `BOARD_LAG`.
   - ListTasks filter: status in a {to-do,in-progress,in-review} + title NOT matching `standing epic` (case-insens).
- Git facts gathered once (all via the existing `git(repoDir, args...)` helper in `boarddone.go` — safe since it's in the same package):
     - branches: `git worktree list --porcelain` → awk branch lines `^branch refs/heads/` stripped → list of live branch names, lowercased (bin line 238 `tolower`).
      - per-worktree ahead subjects: `git -C <wt> log origin/main..HEAD --pretty=%s%n%b`.
      - work_in_flight: any `git -C <wt> status --porcelain` non-empty OR any ahead-of-main commits.
      - merged_log: `git log origin/main -500 --pretty=%ct%x09%s` (Epoch, then tab, then subject).
   - Active/merged flags: branch-name match `(lref|nref)([^0-9]|$)` (bin 295-298; `|$` = end-of-line alternative) against BOTH the lowercased worktree branch list AND the lowercased ahead-subjects blob; merged via the pure matcher; found on a ticket.

2. `pkg/sync/refshipped.go` — the pure matcher, the ONLY code path deciding shipped:
   - `func RefShipped(log string, ref string, createdEpoch int64) bool` — port of `_bsync_ref_shipped`: skip empty subjects; ref must match `\b<ref>\b`; if preceded by any MENTION pivot (see list) → false; the age gate: `ts > 0 && createdEpoch > 0 && ts < createdEpoch → false`; any line surviving all three → true, else false.
- Precomputed `mentionRE` from the verbatim pivot list in the bin (line 97: `\b(after|before|follow-?up|prep(aration)?|refs?|references?|see|per|towards?|related( to)?|unblocks?|blocked by|depends on|part of|child of|parent of|split from|superseded by|replaces)\b[ a-z]{0,15}\b${ref}\b`), followed by the literal ref.
   - `func EpochString(iso string) (int64, bool)` — ISO8601 → unix, port of `_iso_to_epoch` (strip frac + trailing Z, GNU `-d` then BSD `-f`); returns `(0, false)` on total failure, which disables the `created` gate for that ticket.
   - `func NormalizeRef` and the existing pattern: reuse existing zeroPad for `lref`/`nref` derivation (strip `-`).

3. `cmd/herd/main.go` — `case "board-sync": runBoardSync()`:
   - Flags mirror the bin exactly: `--json`, `--daemon`, `--ensure-daemon`, `--cache-check`, `--ship-check`; `--interval` is the SECOND positional arg consumed only after `--daemon`/`--ensure-daemon` (bin lines 48-52: `if ${2:-} == "--interval"`). `--ship-check` (verbatim port of the bin's stdin test hook, lines 104-113: arg = `<ref> [created-epoch]`, read `epoch<TAB>subject` lines on stdin lowercased, print `shipped`/`not-shipped`, exit 0 shipped / 1 not / 2 missing ref). `interval` default `HERD_BOARD_SYNC_INTERVAL:-120`, validated positive integer else exit 2.
   - provider wiring identical to `runBoardDone` (`config.LoadConfig(".herd/herd.yaml")`, switch `cfg.TaskProvider.Type`, `NewKaneoProvider(cfg.TaskProvider.APIURL, cfg.TaskProvider.ProjectID, cfg.TaskProvider.UseCLI)` else `NewMemoryProvider`).
   - exit: `os.Exit(0)` on clean, `os.Exit(3)` on drift>0, `os.Exit(2)` flag usage, `os.Exit(4)` cache-check stale/missing, `os.Exit(1)` provider/env hard error.
   - cache: JSON `{"generated_epoch":<epoch>, "interval":<int>, "exit_code":<rc>, "payload":{"drift":<0|1>, "findings":[…]}}` (bin `_bsync_refresh_cache`, lines 150-171); staleness check (`_bsync_cache_check`, lines 125-148) = file unreadable, invalid JSON, non-integer/missing anonymous, `age < 0`, or `age > 2*interval` → exit 4 + "STALE board-sync cache …"; else prints `board-sync cache age=… drift=… exit=…` + raw findings, exit 0 only when `drift==0 && exit==0`, else exit 3.

Explain why NOT reusing existing evidence prompts:
- `pkg/harvest.Harvester` and its `checkUnmerged` uses `git cherry origin/main` — exactly the tool `herd-lost` refuses ("compare SUBJECTS against origin/main instead, because a subject survives a rebase and a patch-id does not") and `herd-board-sync` also needs subject-based ship detection (not cherry) for the same reason; the board-sync matcher here is subject + boundary + mention + date.
- `pkg/sync.MergeEvidence` uses `git log --grep` which can't express the mention-prexation gap and has no `createdAt` gate; `RefShipped` must. Do NOT reuse `MergeEvidence` production in the recon.

## Acceptance criteria (checkbox)

- [ ] `go test ./pkg/sync -count=1` — all refactored `ReconcileBoard` tests pass; removed assertions on stub's `UpdatedStatus`.
- [ ] Empty board (no tickets or all filtered) → drift 0, no findings.
- [ ] `standing epic` any status is skipped; nothing in findings.
- [ ] in-progress/in-review, not active, merged → `SHIPPED` with suggestion `"verify, then: kaneo task status <tid> done"`.
- [ ] in-progress/in-review, not active, not merged, any worktree dirty/ahead → `UNKNOWN` + "cannot prove dead (a person is not visible but a/p work may be in-flight), do NOT flip to to-do".
- [ ] in-progress/in-review, not active, not merged, NO lane in flight → `STALE`.
- [ ] to-do with live branch-name match → `BOARD-LAG`.
- [ ] `RefShipped` holds the three preconditions exactly as analyzed (acceptance cases below, quoted from zsh).
- [ ] Branch-name ref match only when `(lref|nref)` followed by a non-digit (prevents `FAC-87` flags, e.g. `cha14` must not vote `cha141`; "digit boundary" explicitly).
- [ ] Non-JSON kaneo response exits 1; unknown flag/interval exit 2; `--help` exit 0.
- [ ] `--json` output is valid JSON with exactly `drift` and `findings` keys.
- [ ] Exit 0 drift 0, 3 drift>0; the daemon/cache paths port the same 4/0/3 result for cache-check.

## Acceptance "quoted" (verbatim port, automation harness)

These four cases must be the FIRST `TestRefShipped` table rows, quoted straight from the zsh header (lines 7-18):

| `_bsync_ite` comment | go input `log` | ref | created | want |
| --- | --- | --- | --- | --- |
| "SHIP position" | `1111111111\tFix board-sync drift (CHA-254)` | `cha-254` | 0 | shipped |
| body-only never counts | `log` subject has no ref, body has ref (simulate via subjects-only) | `cha-477` | 0 | not |
| mention-preceded | `...\t8888888888\tr... report refresh getNfts comments after CHA-476\n1111111111\tfeat: ship CHA-476` | `cha-476` | `899000000` | not |
| mention-preceded ship 2 | `\tR2 follow-up on CHA-268` | `cha-268` | 0 | not |
| date gate | `subject predates createdAt` ts=100 < created=500 | `cha-427` | 500 | not |
| ref-reuse rollback fix | old commit (pre-rollback) tags new ref | `cha-427` | later | not |

## Workflow

1. Enter worktree: `cd .herd/worktrees/fac-62`
2. Inspect existing code and understand what needs to change
3. Write failing tests first (TDD)
4. Implement the minimal solution
5. Verify: `go test ./...` (or equivalent test command)
6. Commit with a conventional commit message
7. Signal completion by moving the card to 'in-progress' (review pipeline)

## Role Context

Role prompt from: `.herd/prompts/worker.md`
