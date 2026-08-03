# FAC-136 fleet health metrics

This document describes the bounded health foundation exposed by `pkg/metrics`
and `pkg/server`. It is an observation and operator contract only. Production
event wiring and persistence-store selection remain a later integration stage.

## Signals and thresholds

| Signal | Warning | Critical | Operator meaning |
| --- | ---: | ---: | --- |
| `herd_readiness_ready` | `0` for 5m | `0` for 15m | Critical dependencies are not authoritative and ready. Liveness is separate. |
| `herd_queue_pressure_known` | `0` for 2m | `0` for 10m | Queue depth/capacity is unavailable; do not infer free capacity. |
| `herd_stalled_work` | `>0` for 10m | `>0` for 30m | Aggregate stalled work exists; investigate state age and blocked/backpressured causes. |
| `herd_dropped_callbacks` | Any increase | Any increase for 5m | Callback delivery is losing events; inspect the event path and durable store. |
| `herd_review_saturation_ratio` | `>= 0.80` | `>= 0.80` | The single saturation boundary is 80%; do not treat saturated work as eligible idle. |
| `herd_dead_provider` | `1` for 2m | `1` for 10m | A provider is dead; readiness must remain false until authority is restored. |
| `herd_integration_backlog` | `>0` for 10m | `>0` for 30m | Verified work is waiting for serialized integration. |
| `herd_retries` | Increase over 5m | Increase over 15m | Repeated delivery or transition failures need diagnosis. |
| `herd_dead_letters` | Any increase | Any non-zero value for 10m | Failed work requires explicit operator recovery. |
| `herd_max_lease_age_seconds` | `>900` | `>1800` | Lease ownership may be stale or a worker may be stalled. |
| `herd_max_callback_age_seconds` | `>300` | `>900` | Callback acknowledgement is older than the operational bound. |
| `herd_eligible_idle_seconds` | `>600` | `>1800` | Eligible, non-blocked, non-backpressured work is idle. |
| `herd_last_reconciliation_timestamp_seconds` | age `>300` | age `>900` | Reconciliation has not produced a recent authoritative observation. |

These are starting alert thresholds for the bounded foundation, not measured
SLO commitments. Tune them only with evidence from the eventual event wiring.
The default freshness window for health, queue, signals, and transition SLO
observations is five minutes; callers may inject shorter bounded thresholds.

## Runbook

1. Check `/v1/status` and `/metrics` on the same server instance. Confirm that
   `health.liveness` is true and inspect `health.readiness` independently.
2. If readiness or queue authority is unknown, stop capacity claims. Do not
   convert missing observations, provider errors, or callback errors to zero.
3. A current unhealthy dependency is fresh evidence but is still not ready:
   inspect `freshness.health_fresh`, `freshness.health_ready`, and bounded
   `freshness.reasons` separately. Check `signals.observed_at` and
   `herd_last_reconciliation_timestamp_seconds`. A stale observation is a
   fail-closed condition, not proof of a healthy fleet.
4. Check the bounded `condition_codes` for stalled work, dropped callbacks,
   review saturation, dead provider, integration backlog, dead letters, and
   eligible idle. Use bounded aggregate evidence only; this
   foundation intentionally does not expose task, ref, model, or raw-error
   labels.
5. After an operator-approved recovery, verify a fresh reconciliation and
   fresh signal observation before treating the fleet as ready.

## Restart and persistence

`MetricsExporter` accepts an injectable `StateStore` with `Load` and `Save`
methods. `Persist` and `Restore` are explicit operations so callers can choose
a durable implementation without this slice inventing production wiring.
Restore rejects unsupported schemas, malformed data, contradictory totals,
stale timestamps, and out-of-range values, resetting the exporter to an
unknown state. Eligible idle is derived at each read from `read_at -
eligible_since` only when eligible waiting is positive and work is not blocked,
backpressured, review-saturated at the 80% boundary, or integration-blocked.
A caller cannot persist or claim idle time that contradicts those conditions.
Newer invalid observations leave bounded unknown tombstones carrying their
sequence and observed time, fencing older authority.
