# Task Packet: FAC-82

**Title**: Port herd-review-ledger: append-only JSONL review attempt ledger
**Priority**: no-priority
**Status**: in-progress
**Labels**: 

## Worktree

**Path**: `.herd/worktrees/fac-82`
**Branch**: `task/fac-82-port-herd-review-ledger-append-only-jsonl-review-attempt-ledger`
**Role**: worker
**Agent**: opencode / litellm/lazer/deepseek-v4-flash
**Assigned Worktree**: .worktrees/worker

## Description

# Task Packet: FAC-82 — Port herd-review-ledger (append-only review-attempt ledger + harvest queue) to Go

## Outcome (observable end state)

`herd review-ledger` replaces `bin/herd-review-ledger` 1:1: an append-only JSONL record of every review attempt plus the CHA-657 harvest queue that lives BESIDE the ledger file (CHA-663 — never in the global state dir, so a ledger override can never detach the queue from production state). The ledger is written BY the LAUNCHER at spawn; every event is append-only and never rewritten in place. Subcommands `record`, `tier`, `verdict`, `repair`, `consumed`, `enqueue`, `queue`, `eligible`, `show`, `pending`, `veto-shas`, `pass-shas` keep their exit semantics (eligible 0 harvestable / 1 fatal / 3 not-eligible; usage/arg errors 2; `--help` exit 0) and the exact JSON row shapes and human lines. The ingest side (`bin/herd-review-ingest`) keeps writing verdict rows; `pkg/throughput` (FAC-95) keeps reading `{event:verdict,sha,ts,verdict}` — both byte-compatible.

## Source contract (bin/herd-review-ledger — quoted behavior that MUST survive)

Core invariants (lines 2-15):

```
# herd-review-ledger: durable, append-only record of every review attempt.
# WHY (CHA-656). Review state lived nowhere. CLI-only reviewers such as Kimi are
# invisible to `herdr agent list`, verdicts had no queued/running/consumed state,
# and a PASS did not enqueue anything, so finished pins accumulated while the
# herd honestly reported zero reviewers. On 2026-07-28 six pins sat while an
# exact-SHA PASS went unconsumed.
#
# The ledger is written BY THE LAUNCHER at spawn, never inferred from herdr,
# because "herdr cannot see this reviewer" is precisely the case that broke.
#
# Append-only JSONL, one record per line, newest wins per (sha, reviewer). Never
# rewritten in place: an audit trail that can be edited is not an audit trail.

Path resolution (lines 35-47), with the CRITICAL queue-beside-ledger note:

```
state_dir="${HERD_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/chainseer/herd}"
ledger="${HERD_REVIEW_LEDGER:-$state_dir/review-ledger.jsonl}"
#
# The queue lives BESIDE ITS LEDGER, not in the global state dir (CHA-663).
# Overriding HERD_REVIEW_LEDGER used to move the ledger while leaving the queue
# pointing at real herd state, so every data the queue was the same regardless
# of the override — and toxic reviewers leaked into it.
queue="${HERD_HARVEST_QUEUE:-${ledger:h}/harvest-queue.jsonl}"
```

Coordinator names (line 53): `COORDINATOR_NAMES="${HERD_COORDINATOR_NAMES:-chainseer-orchestrator coordinator}"` — "A reviewer that is the coordinator, or shares the builder's model family, is NOT independent."

Core primitives:
- `norm_sha` (lines 90-94): full object id from `git -C "$repo_root" rev-parse --verify -q "${s}^{commit}"`, else input unchanged — "A short sha and a full sha must not be different reviews."
- event schemas per subcommand (below); all appended with `append()` (line 96) = `print -r -- "$1" >> "$ledger"`.

**`record`** (lines 98-123) — requires `--sha` and `--reviewer` (else exit 2). Family rules:
- `--gate mechanical` → `$(bfam)` must be empty or exactly `mechanical`, else exit 2 ("mechanical record must not carry a builder family other than mechanical"). Mechanical R0's reviewer+gate identity IS the independence proof.
- Otherwise `--builder-family` REQUIRED and must be in the exact 11-token allowlist (line 112): `anthropic|openai|google|xai|zhipu|moonshot|alibaba|deepseek|open-weight|antigravity|proxy`; any other family → exit 2, message `unknown builder family 'X' (refusing unprovable review provenance)`.
- Row (built with jq `-cn`, lines 117-122):
  ```
  {ts,event:"record",sha,branch,builder_family,reviewer_family,reviewer,provider,model,pane,pid,artifact,gate,tier}
  ```
- Prints `herd-review-ledger: recorded launch reviewer=… provider=… sha=…[1,12]`.

**`tier`** (lines 126-134) — newest `record` row's `.tier // ""` for the sha (by ledger order, `tail -1`); empty output when never recorded. Consumers MUST treat empty as R2+ (fail closed): "an auto-harvest that guesses a tier is the self-certify hole the whole ledger exists to close (CHA-977)".

**`verdict`** (lines 136-162) — requires `--sha --reviewer --verdict`; verdict `:u` must be PASS|FAIL|BLOCKED. Row:
```
{ts,event:"verdict",sha,reviewer,verdict:$vd,artifact:$artifact}
  + (if $rf == "" then {} else {reviewer_family:$rf} end)      # only when non-empty
  + (if $bf == "" then {} else {builder_family:$bf} end)
```
Comment (lines 140-144): "Omitted rather than null when unstated, so absent and empty stay distinguishable." Families are REQUIRED here for re-audit ("every row written before this carries no family at all, which is indistinguishable from 'independence was never checked'").
Side-write to the QUEUE (lines 151-162, CHA-657): PASS → append `{ts,event:"enqueue",sha,reviewer,branch,lane,status:"queued"}`; FAIL/BLOCKED → append `{ts,event:"revoked",sha,reviewer,verdict,status:"revoked"}`. Prints `enqueued sha=… for harvest` / `revoked sha=… from harvest queue`.

**`repair`** (lines 164-180, CHA-748) — requires `--sha` + `--reviewer` (the repair AUTHOR). Row `{ts,event:"repair",sha,repair_author,branch}` + optional `repair_family`. Prints "A fresh reviewer for this sha must be neither $reviewer nor [family $rfam nor] the original builder's family."

**`consumed`** (lines 182-190) — requires `--sha`; appends `{ts,event:"consumed",sha,merge_sha:$m}` to the LEDGER and a `{ts,event:"consumed",sha,merge_sha,status:"consumed"}` receipt to the QUEUE (marking queue rows consumed; both files append-only).

**`enqueue`** (lines 192-198) — requires `--sha`; appends `{ts,event:"enqueue",sha,reviewer:${reviewer:-manual},branch,lane,status:"queued"}` to the QUEUE. Manual hook.

**`queue`** (lines 199-227) — the candidate-harvest list, `--json` or human. `QUEUE_FILTER` (204-218), which must survive exactly:
1. `consumed` set = queue rows with event=="consumed" → shas done.
2. `launch` map = newest `record` per (sha, reviewer).
3. `latest` map = newest `verdict` per (sha, reviewer).
4. take queue rows with event=="enqueue", group by sha, keep the NEWEST enqueue per sha (`.[-1]`).
5. drop any sha in `done`.
6. keep shas where latest verdict table (coordinators excluded, launch record present, per the family ladder) has `any(.verdict=="PASS")` and NO `any(FAIL|BLOCKED)`.
Output human line (line 224): `"\(.ts)  \(.sha[0:12])  lane=\(.lane // "-")  branch=\(.branch // "-")" by=\(.reviewer // "?")"`; `--json` emits the raw rows. Errors are swallowed (`|| true`, line 220-225) → empty output is not an error.

**`eligible`** (lines 228-301) — THE HARVEST GATE. Exit 0 = harvestable, exit 3 = refuse (with the two-line stderr on lines 297-299 including "A coordinator self-verification never qualifies."). MUST reproduce the family ladder (lines 274-291) exactly — this is the mission-critical logic:

```
per reviewer $r in the sha's latest verdicts (or EQUALLY the "current" set):
  $g  = launch[$r].gate // "independent"
  $lbf= launch[$r].builder_family // ""
  $lrf= launch[$r].reviewer_family // ""
  $vrf= verdict.reviewer_family // ""      (the row being evaluated)
  $vbf= verdict.builder_family // ""
  $rf = (if $lrf!="" and $vrf!="" and $lrf!=$vrf then null
         elif $lbf!="" and $vbf!="" and $lbf!=$vbf then null       # CONFLICT FAIL-CLOSED
         elif $lrf!="" then $lrf                                   # launch authoritative (R4 repair)
         else $vrf end)                                            # verdict fallback
  then:
    if $g == "mechanical" and ($r == "mechanical" or $rf == "mechanical") then true
    elif $lbf == "" or ($lbf not in the 11-token allowlist) then false
    elif $bf == "" (no --builder-family passed) then true
    else ($rf != null and $rf != "" and $rf != $bf) end
```
Two quoted rules MUST be encoded:
- Line 260-267: "LAUNCH PROVENANCE IS AUTHORITATIVE WHEN PRESENT … launch first, verdict only as the fallback for rows whose launch predates this being recorded." (Live repro CHA-522, f6bad1e3 — launch rows with empty `rf` but verdict rows carrying it were being dropped.)
- Line 269-273 + 287-290: "AND A CONFLICT FAILS CLOSED. Two non-empty values that disagree mean one of them is wrong … `null` marks it … the family test below refuses a null family the same way it refuses an empty one." In jq `null != ""` is TRUE, so entry-point `$rf != null` check must run FIRST (the exact bug the comment references).

**`show`** (lines 302-310) — human rows `"\(.ts)  \(.event|ascii_upcase)  \(.sha[0:12])  \(.reviewer // .merge_sha // "-")  \(.provider // .verdict // "")"`; `--json`: `jq -c select(.sha==$sha)` or whole-file dump.

**`pending`** (lines 312-337) — launched-and-not-yet-a-verdict, ORDER-based not existence-based (line 325 comment: verbatim reasoning "A record is pending when no verdict for its pair appears LATER in the file. Only the newest record per pair is listed"). Newest record per (sha, reviewer); resolved = verdict rows mapped to their ledger indices; pending iff resolution index for the pair < the record's index (or absent). Sorted ascending by ledger key; human line: `"\(.ts)  \(.sha[0:12])  \(.reviewer)  \(.provider // "?")/\(.model // "?")  pane=\(.pane // "-") pid=\(.pid // "-")"`.

**`veto-shas`** (lines 339-361) and **`pass-shas`** (lines 363-394): distinct shas whose current verdict set contains (for veto) any FAIL/BLOCKED or (for pass) any PASS + no veto; same authority rules as eligible (coordinators excluded, launch required). CRITICAL cap policy (quoted 370-380):
> "NEWEST-FIRST BUT COMPLETE, deliberately. This ended in `| head -50`, which read as a harmless bound and was not: the ledger grew past 200 rows and the cap silently dropped genuinely reviewed pins … a merge gate must not decide by recency; if a bound is ever needed it goes on the CONSUMER's walk." — NO cap in the Go port. `pass-shas` sorts newest-first, complete.

Exit-code contract: 0 success/eligible/queue-empty; 1 fatal (die default); 2 usage/arg/validation/unknown-flag/unknown-command; 3 eligible refuses. `-h/--help` prints the usage block (lines 2-30) exit 0.

## Go design (real repo types)

Package `pkg/review` (extends the existing review domain; `FamilyRegistry` already exists in `review.go` but use the ledger's OWN 11-token allowlist — do NOT mix registry families like `grok`/`kimi`/`lazer` into the gate). New files `pkg/review/ledger.go`, `pkg/review/ledger_query.go`, `pkg/review/ledger_test.go`.

```go
type LedgerEvent string // "record" "verdict" "repair" "consumed"
type Verdict string     // "PASS" "FAIL" "BLOCKED"

type LedgerRow struct {  // JSON field order must match the jq-emitted bytes
	Timestamp          string `json:"ts"`
	Event              string `json:"event"`
	SHA                string `json:"sha,omitempty"`
	Branch             string `json:"branch,omitempty"`
	BuilderFamily      string `json:"builder_family,omitempty"`
	ReviewerFamily     string `json:"reviewer_family,omitempty"`
	Reviewer           string `json:"reviewer,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Pane               string `json:"pane,omitempty"`
	Pid                string `json:"pid,omitempty"`
	Artifact           string `json:"artifact,omitempty"`
	Gate               string `json:"gate,omitempty"`
	Tier               string `json:"tier,omitempty"`
	Verdict            string `json:"verdict,omitempty"`
	RepairAuthor       string `json:"repair_author,omitempty"`
	RepairFamily       string `json:"repair_family,omitempty"`
	Lane               string `json:"lane,omitempty"`
	MergeSHA           string `json:"merge_sha,omitempty"`
	Status             string `json:"status,omitempty"`
}

type Ledger struct {
	RepoRoot string
	Path     string          // ledger file
	QueuePath string         // HARVEST queue, ALWAYS dir(Path)+"/harvest-queue.jsonl"
	Now      func() time.Time // = time.Now; injectable for tests
	Coordinators map[string]struct{} // from HERD_COORDINATOR_NAMES
}
func NewReviewLedger(repoRoot, ledgerPath string) (*Ledger, error) // derives QueuePath = filepath.Join(filepath.Dir(ledgerPath),"harvest-queue.jsonl"); creates file (`: >`) if missing
```

Struct fields for the state queries (all pure, read-only over the full row slice):
- `type Keyed = map[string]LedgerRow` helper `newestBy(rows []LedgerRow, key func(*LedgerRow) string)` to build launch/latest/resolved maps.
- `func (l *Ledger) NormalizeSHA(ctx, s string) string` → `git -C repo rev-parse --verify -q s+^{commit}` fallback to input.
- `func (l *Ledger) Record(...) error`, `Verding(...) (enqueued bool, err error)`, `Repair`, `Consumed`, `Enqueue`, `Tier(sha) (string, error)`.
- `func (l *Ledger) Eligible(sha, builderFamily string) (ok bool, err error)`: returns false + error "coordinator self-verification..." style to the caller — exit 3 branch is owned by the command layer.
- `func (l *Ledger) Queued() ([]LedgerRow, error)`, `func (l *Ledger) Pending() ([]LedgerRow, error)`, `func (l *Ledger) PassSHAs() ([]string, error)`, `func (l *Ledger) VetoSHAs() ([]string, error)`.
- `familyResolve` struct to carry the 3-state (set / empty / CONFLICT) so the `null != ""` jq bug maps to a first-class bit.

CLI wiring — `cmd/herd/main.go` `case "review-ledger": runReviewLedger()`:
- Dispatch on `os.Args[2]` (the subcommand); hand-rolled flag loop mirroring the zsh `--flag "$2"` (NOT Go's `flag` package — the zsh parser is position-agnostic with its own unknown-arg handling that must be reproduced).
- Path resolution reuses `stateDir()`/`firstEnv()` (main.go:1754/1744) but adds the `HERD_STATE_DIR`/default `chainseer/herd` composite exactly as the script; queue path derived from the resolved ledger path (never another env).
- `now_iso` injection: a package var `var now = time.Now` used by every appender.
- Exit mappers exit codes 0/1/2/3 per subcommand per contract.

## Acceptance criteria (checkbox)

- [ ] `go test ./pkg/review -count=1` green including ledger tests.
- [ ] Queue path = dirname(ledger)/harvest-queue.jsonl ALWAYS; HERD_REVIEW_LEDGER override in a test does NOT touch the production queue (CHA-663).
- [ ] `record` enforces mechanical-vs-independent family rules; 11-token allowlist; unknown family exit 2.
- [ ] `tier`: newest recorded tier per sha; empty → "no tier" (treated R2+ by consumers).
- [ ] `verdict` PASS appends enqueue+queued to QUEUE; FAIL/BLOCKED appends revoked; verdict set {PASS,FAIL,BLOCKED}.
- [ ] `repair` emits repair row + the "fresh reviewer must not be author/family/builder-family" notice.
- [ ] `consumed` receipts on both ledger and queue; marks done.
- [ ] `eligible` ladder faithfully reproduces: launch-authoritative families, verdict fallback, conflict→false, mechanical fast-path, `--builder-family` absent → family passes.
- [ ] `eligible` exit 3 + exact stderr when not harvestable, including the coordinator self-verification disallow.
- [ ] `queue` newest-enqueue-per-sha, done-drop, and latest-verdict set logic match QUEUE_FILTER output for a fixture.
- [ ] `pass-shas`/`veto-shas` complete with NO cap and newest-first ordering.
- [ ] Every consumer path (FACM-95 throughput, drain) compares byte-identical to the zsh emitter (fixture-pinned).
- [ ] Missing/unreadable ledger/queue → empty result + exit 0 (no panic; `|| true` equivalents).
- [ ] `-h`/`--help` exit 0; unknown command/flag exit 2.

## Test plan (table-driven, FIRST)

Fixture: a real temp git repo (one commit) so `norm_sha` is real; ledger + queue written via `Ledger` methods; expected bytes compared verbatim to a golden JSON (taken from this script's jq output).

| case | setup | want |
| --- | --- | --- |
| record + allowlist | `--sha --reviewer --builder-family xai` exit 0 | row hash exact |
| record unknown family | `--builder-family zoo` | exit 2 |
| record mechanical | gate=mechanical, bfam=mechanical | ok; row gate=mechanical |
| record mechanical bad | gate=mechanical, bfam=openai | exit 2 |
| verdict PASS enqueues | PASS | queue row event=enqueue status=queued |
| verdict FAIL revokes | FAIL | queue row event=revoked status=revoked |
| tier newest | record tier R2 then R0 same sha | "R2" |
| tier never | none | "" |
| consumed both | consumed sha | == done, absent from queue list |
| pending order | verdict at idx>record idx (same pair) | NOT pending |
| pending newer record after verdict | verdict@i then record@j>i | pending (CHA-904) |
| pending newest-only | two records same pair | one row |
| eligible PASS | independent PASS w/ launch family; `--builder-family xai` mismatch | false (family mismatch) |
| eligible PASS matches | launch bfam=xai, reviewer family != xai | true, exit 0 |
| eligible veto | FAIL present in latest | false exit 3 |
| eligible no bf passed | bf="" | true when launch family allowlisted |
| eligible conflict | launch lrf="x" verdict vrf="y" (both set) | false |
| eligible mechanical | gate=mechanical reviewer=mechanical | true even none independent |
| queue pass not yet vetoed | PASS + no FAIL | queued |
| queue veto present | FAIL latest same sha | dropped |
| pass-shas cap | >50 pass shas, sha at idx 89 | all returned (no cap) |
| byte parity | Go row vs jq fixture | JSON byte-identical |

FIRST: all pure (temp-dir git + static row fixtures), fast (<100ms), Repeatable (deterministic timestamps via `Now`), Self-verifying via exit codes and compare, Timely (runs in a single `go test` invocation).

## Workflow

1. Enter worktree: `cd .herd/worktrees/fac-82`
2. Inspect existing code and understand what needs to change
3. Write failing tests first (TDD)
4. Implement the minimal solution
5. Verify: `go test ./...` (or equivalent test command)
6. Commit with a conventional commit message
7. Signal completion by moving the card to 'in-progress' (review pipeline)

## Role Context

Role prompt from: `.herd/prompts/worker.md`
