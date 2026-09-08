# Standing fence-broker and hook-policy operator runbook (FAC-776)

This is the operator procedure for running `herd standing` against a
standalone fence-broker, and for keeping harness hook policy in sync across
providers. It documents the native (installed) command surface as it exists
today, not the seams described in README remedies or error strings that no
longer match the code.

## 1. Two different authorities — do not mix

| Need | What it is | What it is not |
|---|---|---|
| Standing launch connectivity | The process running `herd standing` is a **client** of an already-running `herd fence-broker`. Before tab create, it validates URL+token, `/healthz`, authenticated status, and claim-dir match. Worker URL/token land in the lane's gitignored `.herd/contain/env.list` (mode 0600). | Not mint. Not self-dispatch. Not `HERD_FENCE_COORDINATOR=1`. |
| Coordinator mint / fenced board write | In-process only. `HERD_FENCE_COORDINATOR=1` makes `OpenClaimStack` start a broker, generate worker+mint tokens in memory, and attach a minter. Credentials never leave that process. | Cannot feed standing children. Same claim-dir flock as a standalone broker — one live broker per volume. |

`herd pulse --act --spawn` does wire a real dispatch path (`gatherPulseObservation`
-> `actor.dispatch` -> `dispatchTicketDecision`, `cmd/herd/pulse.go` lines
132-148); the heartbeat comment on that file does not mean pulse can't
dispatch. The actual blocker is different: pulse's dispatch still can't mint a
claim next to a standalone broker, because a production remote minter isn't
available to a client-only coordinator, and an in-process coordinator broker
takes the same claim-dir flock a standalone broker holds. Don't route around
that by weakening mint, and don't claim pulse never dispatches — it does; it
just can't win the mint race under a standalone broker.

## 2. Command surface (exact — no invented flags)

`runFenceProvision` / `runFenceBroker` read **environment only**. They do not
parse `--claim-dir`, despite what the following currently say:

- the README remedy text
- `pkg/preflight/fence_broker.go`
- `ErrWorkerBrokerMissing` / `ErrWorkerBrokerInProcess` error strings
- `clihelp.go`'s `herd fence-broker [flags]` / `herd fence-provision [flags]`

Native commands that exist and take no such flags:

```zsh
herd fence-provision
herd fence-broker
herd standing
herd preflight
```

`herd preflight` only **warns** when there's no broker URL/coordinator/
atomic-server; unlike `herd standing`, it does not Live/Status-check and does
not fail the raise.

## 3. Existing sealed claim volume vs. a fresh one

`CanonicalClaimDir` resolves to `HERD_CLAIM_DIR` if set, else
`<canonical-root>/.herd/claim`.

### Already sealed (`fences.db` has a `store_authority.volume_seal` row)

Do **not** run `herd fence-provision` against it. The first-time path unsets
`HERD_FENCE_VOLUME_ID`, then `WriteSharedMarker` refuses to overwrite an
existing seal (`store already sealed … refuse overwrite —
HERD_FENCE_ROTATE=1 with matching HERD_FENCE_VOLUME_ID`).

Do **not** set `HERD_FENCE_ROTATE=1` to "recover" the seal. Rotate mints a
*new* seal after proving the env matches the current DB row — that's
rotation, not recovery, and it invalidates every client still holding the old
value.

**Recovering an existing seal without rotating:**

- Authority is the `fences.db` row `store_authority.volume_seal`, matched
  against process env `HERD_FENCE_VOLUME_ID` (min 32 chars).
- The `SHARED` pointer file is 0644 and contains only `claim_dir=…` — the
  seal is never written there.
- Prefer reusing the value from the original provision print (the value the
  operator already distributed when the volume was first sealed).
- There is no native "print existing seal" CLI seam (`ReadVolumeSeal` only
  runs *after* a successful `WriteSharedMarker`). If the original print is
  lost, a coordinator may recover the value directly into the broker
  process's own environment with a read-only sqlite query — this reads the
  existing row, it does not write to the database or change claim/seal
  authority:

  ```zsh
  HERD_FENCE_VOLUME_ID="$(python3 - <<'PY'
  import os, sqlite3
  from pathlib import Path
  p = Path(os.environ['HERD_CLAIM_DIR']) / 'fences.db'
  db = sqlite3.connect(p.as_uri() + '?mode=ro', uri=True)
  rows = db.execute('SELECT volume_seal FROM store_authority WHERE id=1').fetchall()
  assert len(rows) == 1 and len(rows[0][0]) >= 32
  print(rows[0][0])
  PY
  )" || exit 1
  export HERD_FENCE_VOLUME_ID
  ```

  Capture the value directly into export via `||exit 1` on the substitution —
  never `export X=$(cmd)` unguarded, since that form swallows a failed
  substitution and exports an empty string instead of stopping. Never print,
  log, or paste this value into a ticket, packet, or this runbook.

### Fresh volume (no `volume_seal` row yet)

```zsh
unset HERD_FENCE_VOLUME_ID HERD_FENCE_PROVISION_TOKEN
export HERD_CLAIM_DIR="<abs-claim-dir>"
herd fence-provision
```

This sets `HERD_FENCE_PROVISION=1`, mints a random 32-byte hex seal, writes
`fences.db` + `SHARED`, then prints `export HERD_CLAIM_DIR=…` and
`export HERD_FENCE_VOLUME_ID=…`. Treat that print as secret; do not commit
it. A pre-set `HERD_FENCE_VOLUME_ID` at first-time provision is refused
(stolen-seal split-brain protection).

## 4. Worker/mint credentials

There is no `herd` command that mints these. `StartFenceBroker` requires:

- `HERD_FENCE_BROKER_TOKEN` — non-empty, length ≥ 16 (the **worker** token)
- `HERD_FENCE_BROKER_MINT_TOKEN` — non-empty, length ≥ 16, must differ from
  the worker token

The operator generates two independent high-entropy secrets (any local
generator works, e.g. `python3 -c 'import secrets; print(secrets.token_hex(32))'`
run twice). Store both in a protected file, mode `0600`, **outside the
repository** — never in git, packets, herdr `--env`, or the standing
`env.list`. Only the worker token, never the mint token, is ever exported to
a worker/standing client.

`herd fence-broker`'s success stdout reprints `export
HERD_FENCE_BROKER_TOKEN=…` — that stdout is secret-bearing; capture it
privately, don't log it. The mint token is not printed by the broker.
`reconcileMintCredential` removes a stale `fence-mint.cred` file rather than
writing one — despite an in-code comment suggesting mint is file-backed, the
live mint for a standalone broker is only the broker process's own
`HERD_FENCE_BROKER_MINT_TOKEN` env, never a worker-readable file.

An in-process coordinator (`StartCoordinatorBroker`) generates both tokens in
memory and never places them in env — that path is not a token source for
standing.

## 5. Upstream task-provider config

A standalone `herd fence-broker` does **not** read `.herd/herd.yaml`. Upstream
must be supplied explicitly via env:

| Env | Role |
|---|---|
| `KANEO_API_URL` (else `HERD_FENCE_UPSTREAM_URL`) | board API origin |
| `KANEO_PROJECT_ID` (else `HERD_FENCE_UPSTREAM_PROJECT`) | project |
| `HERD_FENCE_UPSTREAM_CLI=1` | use the Kaneo CLI upstream path |

The in-process coordinator broker copies these from its already-constructed
Kaneo provider; a standalone process sees none of that and needs the env set
directly. The standing **raise** gate itself only needs broker liveness +
worker-token pairing — upstream env matters for the broker's later fenced
proxy writes, not for the raise.

Listen address (optional):

```zsh
export HERD_FENCE_BROKER_LISTEN=unix          # default: unix socket <claim-dir>/fence-broker.sock
# or, if the unix path would exceed Darwin's ~104-byte limit:
export HERD_FENCE_BROKER_LISTEN=127.0.0.1:0
```

## 6. Start the standalone broker (one owner per claim volume)

```zsh
export HERD_CLAIM_DIR="<abs-claim-dir>"
export HERD_FENCE_VOLUME_ID="<existing-distributed-seal>"   # required once sealed
export HERD_FENCE_BROKER_TOKEN="<worker-secret-min-16>"
export HERD_FENCE_BROKER_MINT_TOKEN="<distinct-mint-secret-min-16>"
# optional listen + upstream env from §5
herd fence-broker
```

Long-running; holds an exclusive `flock` on `<claim-dir>/fence-broker.lock`.
A second owner on the same volume fails with `exclusive flock failed
(another live broker on this claim volume)`.

On listen it prints `HERD_FENCE_BROKER_URL` (`unix://<sock>` or
`http://127.0.0.1:<port>`) — that's the URL standing and other clients use.

## 7. What `herd standing` actually checks before tab create

For any lane with capability `board-write` (unless
`HERD_FENCE_ATOMIC_SERVER=1`):

1. `HERD_FENCE_COORDINATOR=1` set → refuse (`ErrWorkerBrokerInProcess`, no
   shareable route).
2. Missing URL or token → refuse (`ErrWorkerBrokerMissing`).
3. Token shorter than 16 chars, or mint token equal to worker token → refuse.
4. `NewFenceBrokerClientFromEnv` → `Live` (`GET /healthz`) → `Status` (`GET`
   the shared status path, using the **worker** token).
5. Broker's `claim_dir` must equal this repo's `CanonicalClaimDir`.
6. If `HERD_FENCE_VOLUME_ID` is set client-side, `ValidateSharedMarker` must
   pass.

Read-only lanes (no `board-write`) skip all of this.

## 8. Env for `herd standing` (client of the standalone broker)

```zsh
unset HERD_FENCE_COORDINATOR
export HERD_CLAIM_DIR="<same-abs-claim-dir-as-broker>"
export HERD_FENCE_VOLUME_ID="<same-seal-as-broker>"   # if the broker required it
export HERD_FENCE_BROKER_URL="<printed listen URL>"
export HERD_FENCE_BROKER_TOKEN="<same worker token — never mint>"
herd standing
```

Never put the mint token, `HERD_FENCE_COORDINATOR`, or the volume seal into a
lane's child env. `herd standing` writes **worker URL + token only** into
`<lane-cwd>/.herd/contain/env.list` (mode 0600, gitignored); `herdr tab
create --env` carries workspace identity (`HERD_ROOT=<lane-cwd>`), not
tokens; the child's own `OpenClaimStack` / `NewFenceBrokerClientFromEnv`
loads that file via `HERD_ROOT`.

## 9. Pulse / in-process claim vs. the standalone owner

| Mechanism | Starts a broker? | Shareable worker route? | Compatible with a standalone owner on the same volume? |
|---|---|---|---|
| `herd pulse --act` | No | No | N/A — heartbeat only |
| `herd pulse --act --spawn` (`dispatchTicketDecision`) | No | No | Dispatches real work, but still can't mint against a standalone broker (§1) |
| `herd daemon` `RunPulse` claim | Only if that process also has `HERD_FENCE_COORDINATOR=1` | No — tokens stay in-process | Incompatible: takes the claim-dir flock |
| `HERD_FENCE_COORDINATOR=1` + `HERD_FENCE_BROKER_URL` set together | Refused (`standalone broker owns this claim volume`) | No | Explicitly exclusive |
| Standalone `herd fence-broker` + clients (URL+TOKEN, coordinator unset) | One owner | Yes — this is the FAC-776 standing path | Compatible, as long as coordinator/daemon stay clients |

Do not enable `HERD_FENCE_COORDINATOR=1` to "fix" standing — that creates a
second, incompatible owner. A production fenced Kaneo mutate still needs a
minter (`missingMintCapabilityError` otherwise); that gap is not solved by
standing's worker-token injection, and standing's raise can succeed while a
later fenced `board-done` in a client-only coordinator still refuses to mint.
This is a real, open product gap (§1) — do not paper over it by exporting
mint credentials into a lane.

## 10. Suggested operator sequence — existing sealed volume

1. Confirm the claim dir and that `fences.db` already has a seal row. Do not
   provision.
2. Restore `HERD_CLAIM_DIR` + the previously distributed
   `HERD_FENCE_VOLUME_ID` into the **broker** process only (§3 — no
   rotation, no DB write).
3. Generate two distinct ≥16-char tokens; keep the mint token on the broker
   process only.
4. `herd fence-broker` with the env from §6. Leave it running.
5. Copy URL + worker token (never mint) into the coordinator/standing
   launcher env; `unset HERD_FENCE_COORDINATOR`.
6. `herd standing`. Board-write lanes should pass the authorize-before-tab
   check; read-only lanes skip it as always.

Failure triage:

- Step 4 fails on seal mismatch → the env seal doesn't match the DB row;
  recover the distributed value (§3), don't rotate unless a fleet-wide seal
  change is actually intended.
- Step 4 fails on flock → another process (often an in-process coordinator)
  already owns this volume; stop that owner rather than starting a second
  broker.
- Step 6 still reports in-process / no shareable route → something in the
  standing process still has `HERD_FENCE_COORDINATOR=1` set.

### Chainseer example (concrete, current)

```zsh
export CHAINSEER_ROOT="<chainseer-checkout>"
export HERD_CLAIM_DIR="$CHAINSEER_ROOT/.herd/claim"   # already sealed
export KANEO_API_URL=https://kanban-api.kampe.kluster
export KANEO_PROJECT_ID=ypsjln1upv5rbxapxbr4mluz
export HERD_FENCE_UPSTREAM_CLI=1
```

`use_cli: true` in this project's `herd.yaml` is what the in-process
coordinator broker copies automatically; a standalone broker needs the
`HERD_FENCE_UPSTREAM_CLI=1` env above instead.

## 11. Hook policy across providers

The companion inventory command is built from an isolated checkout of the
exact installed source, not the working tree:

```zsh
go build -o "$HOME/.local/bin/herd-hook-inventory" ./cmd/herd-hook-inventory
```

`HERD_HARNESS_HOOKS_FILE` overrides the repo's `.herd/harness-hooks.json` for
whichever process reads it. Claude's own hook discovery reads **both** the
user's global settings and the current repo's project settings — so a policy
file has to be scoped to the right host *and* the right repo; a policy tuned
for one repo can't just be copied onto another with a different hook set.

Rollout for a policy change:

```zsh
herd hooks-pin --provider claude --file "$policy" --dry-run
# explicitly review every ADD/DROP the dry-run reports
herd hooks-pin --provider claude --file "$policy"
"$HOME/.local/bin/herd-hook-inventory" --provider claude --validate
```

The native pin computes its own revision; there's no manual digest math.
Preserve existing classifications across a rollout — the generator defaults
newly-added hooks to *optional*, which is a reasonable default but not a
license to blanket-enable them without reviewing what each one actually
does.

Grok and Codex, absent a provider-specific policy, report no hooks and
validate cleanly by default — that's expected discovery behavior for those
providers, not evidence that hooks fail to run for them. Don't fabricate a
per-provider digest table to make the three providers look symmetric when
they aren't.

### Chainseer example (concrete, current)

A reviewed policy for Chainseer exists at
`$HOME/.local/state/herdforge/hook-policy/chainseer-claude-20260908.json`: 31
entries, preserving the 29 existing host rows and adding two new project
rows — `role-inject` (`SessionStart`) and a fail-open `goal-guard` (`Stop`) —
both individually assessed as optional continuation/context hooks with no
change to any existing safety classification. Validation for Claude, Grok,
and Codex passed against this policy from a Chainseer checkout at the
audited source revision. No Chainseer broker has been started as part of
producing this runbook or that policy.

## 12. What standing does *not* grant

Getting `herd standing` to raise cleanly against a live standalone broker is
a launch-connectivity fix, not a claim-authority change. It does not:

- authorize a lane to self-claim tickets,
- solve the external-coordinator-mint gap described in §1/§9, or
- substitute for `HERD_FENCE_ATOMIC_SERVER=1` / any other mint bypass.

Those remain open product gaps to fix in source and review, not operator
workarounds to route around here.
