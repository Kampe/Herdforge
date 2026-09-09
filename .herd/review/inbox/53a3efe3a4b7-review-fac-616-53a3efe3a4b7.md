sha: 53a3efe3a4b754a1850164dd3bfc078093072134
branch: recovery/fac-616-flash-production-admission
task: FAC-616
reviewer: review-fac-616-53a3efe3a4b7
reviewer-family: openai
builder-family: open-weight
verdict: PASS
reviewed-base: 93d9c2ba42034d18938adcb28d584790adc0dd91
reviewed-head: 53a3efe3a4b754a1850164dd3bfc078093072134
---

## Review

Risk tier: R2. Full-range patch ID: 0343b02bdf2c5da8ac1420fb36cbd525a102daac.

PASS: no blocking findings remain for the exact reviewed revision. The standing
admission tests exercise the production `runStandingConfigMode` -> `standing.Run`
-> `AdmitRoute` -> `launchAdmission` -> router decision path. Unknown/empty and
stale quota remain routable when the provider is otherwise launchable; measured
exhaustion refuses; dry-run keeps side effects disabled; and production defaults
retain the routed decision seam. The restored mutation control reintroduced the
old quota-only gate and made both positive admission tests fail, then the
candidate was restored cleanly.

Findings: none.

Graph evidence: full graph rebuilt at the reviewed head; 18,110 nodes, 234,375
edges, 1,559 files. The changed production path was traced through its callers and
callees; the graph's initial test-gap report for `runStandingConfigMode` is
covered by the candidate's end-to-end tests.

## Tests run

- `go test ./cmd/herd -run 'TestStandingAdmissionProductionPath|TestStandingLaneWithSpentPreferenceReroutesInsteadOfLaunchingIntoZero|TestStandingQuotaAdmission' -count=1` — exit 0
- `go test -race ./cmd/herd -run 'TestStandingAdmissionProductionPath|TestStandingLaneWithSpentPreferenceReroutesInsteadOfLaunchingIntoZero' -count=1` — exit 0
- `go test ./cmd/herd -run 'TestStanding' -count=1` — exit 0
- `go test ./cmd/herd -count=1` — exit 0
- `go test ./pkg/router -count=1` — exit 0
- `go build -o /tmp/herd-fac616-review ./cmd/herd` — exit 0
- `go vet ./cmd/herd` — exit 0
- `go run ./scripts/hermeticity/` — exit 0
- `git diff --check 93d9c2ba42034d18938adcb28d584790adc0dd91..53a3efe3a4b754a1850164dd3bfc078093072134` — exit 0
- Mutation control: temporarily restored the old `admitStandingQuota(lane)` gate; `go test ./cmd/herd -run 'TestStandingAdmissionProductionPath' -count=1` — exit 1 as expected; candidate source restored and worktree clean.

Residual risk: coordinator-owned broad `make ci` was not duplicated, per packet.

reviewed_at: 2026-09-09T16:47:32Z
