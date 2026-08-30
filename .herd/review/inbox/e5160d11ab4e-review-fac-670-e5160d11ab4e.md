sha: e5160d11ab4e1c18cf82660efd6294eb64c96f96
branch: herd/fac-670
task: FAC-670
reviewer: reviewer
reviewer-family: xai
builder-family: openai
verdict: FAIL
reviewed-head: e5160d11ab4e1c18cf82660efd6294eb64c96f96
---

# FAC-670 review of e5160d11ab4e (R2)

Isolation: `git rev-parse --show-toplevel` resolved to the leased pool slot, not the shared checkout. HEAD == candidate SHA `e5160d11ab4e1c18cf82660efd6294eb64c96f96`. Working tree clean. Merge-base with origin/main: `10c97892751e0d478ffcff85f3f96e11c6e560fd`. Graph at this SHA: 1396 files, 16627 nodes.

Targeted suite was run to completion on this turn (bounded wait, 79.57s, exit 1). It is not unfinished.

## Findings

### 1. [P1] Pin WSL quota-handoff auto-detect in hermetic tests — pkg/usage/usage.go:219

`quotaHandoffRequired()` treats any Linux host whose `/proc/sys/kernel/osrelease` contains `microsoft` as a required-handoff host unless `HERD_QUOTA_HANDOFF_REQUIRED` is set. The function comment says that override "keeps tests hermetic", but helpers that are not asserting the handoff contract never pin it.

This host is WSL2 (`microsoft-standard-WSL2`). Re-run of the card's verification command:

```
go test -count=1 ./pkg/usage ./pkg/router ./cmd/herd
```

`pkg/usage` FAIL, `pkg/router` ok, `cmd/herd` FAIL. Introduced proofs of FAC-670 fail closed on the target host:

- `TestExactGrokReviewAcceptsFreshFlatRateEntitlement` — fresh flat-rate Grok review rejected with `quota handoff unavailable: OpenUsage quota handoff rejected: schema "", want openusage.limits.v1`.
- `TestQuotaJSONOmitsInventedFlatRateMeasurements` — `grok reason = "quota-handoff-error", want "unmetered"` and `available = false, want true`.

The same unpinned auto-detect masks other gates this change claims to preserve. `installExactReviewRouteFixture` does not set `HERD_QUOTA_HANDOFF_REQUIRED=0`, so:

- `TestExactReviewModelOverrideRefusalsAreFailClosed/no_quota_data` no longer reports `UNKNOWN quota`.
- `.../excluded_exact_family` no longer reports `family google excluded`.
- Launch-policy and cache reuse tests fail with `openusage exec: exit status 127` or skip a held snapshot because it cannot pass `validateQuotaHandoffSnapshot`.

Handoff-positive tests that already set `HERD_QUOTA_HANDOFF_REQUIRED=1` passed. `pkg/router` passed because those tests inject `BurnState` and never call `FetchSnapshot`.

Correction: pin `HERD_QUOTA_HANDOFF_REQUIRED=0` and clear `HERD_QUOTA_HANDOFF_BIN` in every quota helper that is not asserting the handoff contract. Keep `=1` only on the handoff-positive cases. Re-run the targeted suite on W4 and watch the named refusal tests go RED against their own regressions, then GREEN on the fix.

### 2. [P2] Spark live-count is model-exact, so occupied Codex still looks like a free slot — cmd/herd/main.go:7471 and pkg/router/route.go:1008

Live W4 evidence after a fresh OpenUsage read (Codex 78 percent remaining): both installed main and this candidate selected `codex/gpt-5.3-codex-spark` with availability `live=0 cap=3` while the operator census reports Codex concurrency occupied.

That pairing is internally consistent with the current code, and it is not a quota-vs-concurrency label mixup:

- `available()` uses quota only to refuse a known-unhealthy pool and to size `cap` via `credits.ClassConcurrency`. 78 percent remaining is an underspent class, so `cap=3`. That is quota truth, not a free-slot claim.
- The free-slot claim is `live=0`. Production `liveRouteCount` counts only `working`/`starting` agents whose pane argv model and `quotasup.QuotaPool` match the exact routed tuple. Occupied Codex on `gpt-5.6-luna`/`default` does not increment spark. Spark therefore advertises `available live=0 cap=3`.
- Spark fallback in `Pick` still requires the word `exhausted` in the default-pool detail. A concurrency-cap refusal on luna does not itself promote spark. Spark is selected because it is the routed model or because its own `available()` succeeded.

`git diff` against origin/main shows no change to `liveRouteCount`. The same spark `live=0 cap=3` selection on installed main confirms this is pre-existing census scope, not a FAC-670 regression.

What this candidate did add is machine-readable `rejections[].gate=concurrency` when `LiveCount` reports occupancy. `TestMachineRouteRecordsConcurrencySeparatelyFromHealthyQuota` stubs Codex `LiveCount` as 3 for every model, so it never sees the production split (luna occupied, spark live=0). The JSON gate is truthful only for the occupancy number it is given; it does not stop healthy Codex quota plus a model-exact zero count from looking like a free spark slot.

Correction if this card is meant to make W4 concurrency honest against the operator census: count live Codex agents at the provider (or shared-account) scope used by the census, or reject spark when default Codex is already at cap. If pool-scoped live counts are intentional, the route reason must name that the occupied Codex processes are on a different model/pool so an operator does not read `live=0 cap=3` as an empty Codex account.

## What held

- Grok probe argv is the documented headless contract (`-p`, `--no-subagents`, `--disable-web-search`, empty stdin). `TestGrokProviderProbeUsesTheHeadlessSinglePromptContract` passed.
- Native Grok billing distinguishes metered, unmetered, and HTTP/auth/decode failure. `TestGrokBillingZeroTotal`, `TestGrokBillingExhausted`, and `TestGrokBillingFailuresRemainUnknown` passed.
- Explicit handoff-required tests that pin the env passed (stale/corrupt/ambiguous, timeout bound, WSL fail-closed without blaming a provider, cache must not restamp source `generatedAt`).
- When `LiveCount` reports occupancy, the new rejection JSON names `gate=concurrency` and does not put `quota` in the detail. That unit path is green; it is not the live spark path above.

## Verification

```
go test -count=1 ./pkg/usage ./pkg/router ./cmd/herd
```

Completed this turn in 79.57s, exit 1. `pkg/router` ok. `pkg/usage` and `cmd/herd` FAIL as listed. Host osrelease is WSL2. Neither `HERD_QUOTA_HANDOFF_REQUIRED` nor `HERD_QUOTA_HANDOFF_BIN` was set in the reviewer environment.

## Residual risk

After the test pin, W4 live Grok admission still depends on a valid OpenUsage handoff payload; native unmetered evidence is diagnostic-only while WSL auto-detect is on. Probe coverage is argv-level, not a live non-TTY grok exec in this review. Codex occupancy vs spark `live=0` remains on main and on this candidate until census scope is changed.

risk_tier: R2
author_family: openai
reviewer_family: xai
candidate_sha: e5160d11ab4e1c18cf82660efd6294eb64c96f96
verification_digest: go-test-count1-pkg-usage-pkg-router-cmd-herd-FAIL-79s-WSL2
reviewed_at: 2026-08-30T16:27:18Z
