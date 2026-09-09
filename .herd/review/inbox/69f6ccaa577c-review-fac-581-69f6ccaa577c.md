sha: 69f6ccaa577cbe14d30c206bbd3b3fdb551a0295
branch: recovery/fac-581-broker-recovery
task: FAC-581
reviewer: review-fac-581-69f6ccaa577cbe14d30c206bbd3b3fdb551a0295
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-head: 69f6ccaa577cbe14d30c206bbd3b3fdb551a0295
---

patch_id: 76925aff73caf454994839b5901cfa098c03323e
verification_digest: sha256:5a5053e222c9d33280697f9a143fb26e6148805db8d5fced1f533ef16f811b41
risk_tier: R3
reviewed_at: 2026-09-09T18:51:37Z

## Findings

1. [Blocker] Resolved dependencies cannot become ready in the production pulse selector. `collectPulseProviderObservation` computes `doneRefs` but calls `selectPulseDispatchTask` without it, and `pulseDispatchDecision` constructs broker inputs without `ClosedTasks` (`cmd/herd/pulse.go:289-334`). Consequently every parsed blocking edge is evaluated against a nil closed-task set and remains blocked forever. A temporary compiling regression with a completed `FAC-136` and dependent `FAC-75` produced no next task. Pass the authoritative closed-task snapshot into the broker decision and add the resolved-dependency transition to the production-caller test.

2. [High] Invalid dependency provenance is silently converted into a ready task. `pulseBrokerQueue` only consumes edges when `ExtractProvenanceFromText` returns no error; malformed, conflicting, or unclosed `herd-deps-v1` fences are otherwise emitted with no dependency and can dispatch (`cmd/herd/pulse.go:376-394`). A temporary compiling regression dispatched a task with `{malformed}` provenance. This violates the fail-closed unknown/error contract and can launch work whose dependency state is not authoritative.

3. [High] Pulse ordering is lexical rather than the required numeric ticket order. The new queue assigns priority ranks and delegates to `broker.Decide`, whose comparator uses plain `Ref <`; the existing `provider.CompareRefs` provides numeric ordering (`pkg/provider/refs.go:8-25`). A temporary compiling regression with equal-priority `FAC-10` and `FAC-3` selected `FAC-10`, violating priority DESC / ticket ASC.

4. [Blocker] The production pulse control path discards the broker decision and still treats a claimable count as dispatch authority. `selectPulseDispatchTask` converts a valid `broker.Decision` into only `*provider.Task` or nil, while `collectPulseProviderObservation` records only `Claimable` and `NextTaskRef` (`cmd/herd/pulse.go:289-321`). The downstream planner still emits dispatch work from `Claimable > 0` (`pkg/pulse/pulse.go:1023-1037`), so an all-blocked or identityless queue has no named wait reason and may produce a misleading would-run/action. Independently, review-only saturation sets `BuilderDispatchBlocked` false in planning but `Apply` rejects the resulting action using global `DispatchBlocked` (`pkg/pulse/pulse.go:977-979,1287-1293`); a temporary compiling production-path probe failed with `pulse: one or more act steps failed`. The broker record must remain the consumed control result through planning and apply, with separate builder/review admission at the final mutation gate.

5. [High] Goal-guard event-wait handling is neither durable nor wired into the normal stop-hook path. `Evaluate` reconstructs a transient `progress.Record` from `Evidence` and never persists the observed artifact (`pkg/goalguard/goalguard.go:257-280`); repeating the same evidence with `LastArtifact=sha-before` and `Artifact=sha-after` consumed a second continuation in a temporary compiling regression. In addition, the production stop hook constructs `Evidence` without `ProgressClass`, `LastArtifact`, or `Artifact` (`cmd/herd/goalguard.go:146`), so ordinary goal-guard operation never reaches the new event-wait branch. Persist or otherwise bind the artifact/cursor across evaluations and populate it from the actual production observation path.

6. [High] Several claimed consumers are test-only helpers, not production wiring. The graph found no non-test callers for `ScoutDecision` or `attention.ClassifyProgress`, and `NextPicker` still presents the `ClaimPreview` count/description without calling `ClaimPreview.Decision` (`pkg/next/scout.go:79-115`, `pkg/attention/attention.go:102-116`, `pkg/next/next.go:159-178`). This leaves parallel count/status decisions in the existing conveyor while the new broker seam is exercised primarily by fixtures. Route the existing scout, attention, and next/claim decisions through the broker or explicitly narrow the acceptance scope.

## Tests run

- `go version` -> `go1.26.6 darwin/arm64` (exit 0)
- `go test ./cmd/herd -run 'TestSelectPulseDispatchTask|TestPulseDispatchDecision|TestWorkBrokerDecisionConformance|TestCollectPulseProviderObservationSelectsReadyBuilderOverBlockedUrgent' -count=1` -> PASS (exit 0)
- `go test ./pkg/next ./pkg/dispatch ./pkg/claim ./pkg/goalguard ./pkg/attention ./cmd/herd` -> PASS (exit 0; cmd/herd 291.427s)
- `go test -race ./pkg/next ./pkg/dispatch ./pkg/claim ./pkg/goalguard ./pkg/attention -count=1` -> PASS (exit 0)
- `go test -race -count=1 -run 'TestSelectPulseDispatchTask|TestPulseDispatchDecision|TestWorkBrokerDecisionConformance|TestCollectPulseProviderObservationSelectsReadyBuilderOverBlockedUrgent' ./cmd/herd` -> PASS (exit 0)
- `go run ./scripts/hermeticity/` -> PASS (exit 0)
- `go build ./...` -> PASS (exit 0)
- `go vet ./...` -> PASS (exit 0)
- `make preflight` -> PASS (exit 0; local warning: no fence broker)
- Temporary compiling controls were restored and the review surface is clean.

## Acceptance gaps and residual risk

- The retained author report is verified at the supplied `verification_digest`, but the managed FAC-679 verifier receipt was explicitly not run and is unavailable for this review.
- Coordinator-owned clean WSL `make ci` was not run here; no claim is made that it passed.
- The native launch receipt confirms the OpenCode GLM5.3 open-weight builder and accepted recovery lane, but contains no operator-attribution field. Native admission therefore cannot establish operator attribution; this remains an acceptance gap.
- The retained author `make lint all` report records one full-unit failure: the `cmd/herd` test binary exceeded its 300-second cap. The candidate and base reportedly reproduce that host-speed timeout; this review did not rerun the coordinator-owned broad gate.
- No PASS or merge authority is granted.
