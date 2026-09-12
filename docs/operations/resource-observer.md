# Resource observer (FAC-829)

`herd resources --watch` samples CPU and memory on a native clock and publishes
one bounded snapshot. It is an addition to the existing command: without
`--watch`, `herd resources`, `--json`, `--gate` and `--selftest` are unchanged.

## Why it exists

The dedicated performance guard samples inside language-model turns, so its
sample clock is the turn clock. Against a 30s target the measured gaps ran
36-51s, and a hand-maintained report once carried an observation stamped
22:27:57Z beside a publish stamp of 22:20:52Z copied forward from an earlier
iteration. Those are two distinct defects — cadence coupling, and a copied
publish field — and a timer plus a write-time stamp remove both mechanically.

## Usage

```
herd resources --watch [--interval 30s] [--lifetime 12h] [--sample-timeout 15s]
herd resources --observer-status [--json]
```

The observer is a foreground service. It does not fork, detach, or install
anything; the terminal that started it owns it, and SIGINT or SIGTERM stops it.
It prints nothing per tick — the status file is the output.

`--observer-status` is the consumer path. It exits `3` when the snapshot is
missing, expired, terminated, unreadable, or refusing, and `0` only when a
current observation admits.

## What it guarantees, and what it does not

Guaranteed:

- Ticks come from a timer, not from an agent turn.
- The publish time is taken at write time on every write, and a publish that
  would predate the observation it carries is refused.
- History is a 24-entry ring inside the one atomic status object, so the
  artifact is bounded by construction. Free-form strings are capped before
  marshal, and reads are bounded with the mode verified on the descriptor.
- Expiry is the earlier of the cadence deadline and the oldest metric's own
  staleness window, so a slow publication cannot renew aged-out metrics.
- Unknown, stale or unsupported data refuses. A consumer revalidates the
  published readings through the same `Decide` the writer used.

NOT guaranteed:

- This is not a real-time guarantee. A sample that overruns its interval causes
  the next tick to be **dropped**, not queued; drops are counted and published.
- A non-cooperative probe cannot be forcibly stopped. `runProbeCtx` bounds what
  *this* process spends — capped capture, `WaitDelay` on the join — and signals
  nothing it does not own.
- The observer authorizes nothing. It does not clear holds, start jobs, mutate
  boards, stop processes, or claim fleet ownership, and it does not replace the
  live performance guard's authority. No crash prevention is claimed.

## Exclusivity

The lock is canonical and has no flag to override it: a caller-supplied lock
path lets two observers each believe they are the singleton. The scope actually
achieved is resolved at runtime and published with every observation:

| scope | meaning |
| --- | --- |
| `same-user-host` | the state root is home-anchored, so every worktree and clone this user runs resolves the same lock |
| `configured-state-root` | `HERD_STATE_DIR` or `XDG_STATE_HOME` is set; exclusivity covers that root only |
| `per-checkout` | no home directory could be resolved, so the path is relative and another checkout gets its own lock |
| `injected-path` | a caller supplied non-canonical paths; no host-wide or cross-worktree claim is made |

A competing observer refuses promptly and never writes over the incumbent's
report. The observer lock is deliberately distinct from the capacity/reaper
lock, and the observer never holds that lock for its lifetime.

## Verification

`scripts/verify-resource-observer.zsh` is the non-vacuity driver, wired into CI.
It runs the observer suites as baselines, then mutates the real production
source one guard at a time. A mutant counts as killed only when it **compiles**
and its **named** killer test fails with the **named** assertion text, read from
`go test -json`. A compile error, timeout, skip or unrelated assertion is not a
kill. Baselines must pass before any mutant means anything, and again after the
source is restored. Logs are retained as a CI artifact.

## Dependencies

- **PR836 (`f54af3cf`) is a source dependency.** The observer is built on the
  corrected CPU/load and OS memory-pressure admission APIs from that change.
  Before it, the probes and parsers failed open, so a resident sampler would
  have published confidently wrong healthy readings on a cadence.
- **PR830 is an integrated-CI dependency, not a source import.** PR836's Docker
  gate currently fails its source-manifest limit, which PR830 addresses.
