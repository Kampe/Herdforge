sha: 946b006d450bc82e2fba241b235f36f9911eb6e0
branch: recovery/fac-616-live-admission
task: FAC-616
reviewer: review-fac-616-946b006d450b
reviewer-family: openai
builder-family: google
verdict: PASS
reviewed-base: faae45fecc9edd178794c0e639b4fe27a602a42b
reviewed-head: 946b006d450bc82e2fba241b235f36f9911eb6e0
---

risk_tier: R3
reviewed_at: 2026-09-09T14:51:36Z

The exact candidate is one commit with 3 modified files: `pkg/usage/quota.go`,
`pkg/usage/quota_test.go`, and `cmd/herd/launch_policy_test.go` (151 insertions,
11 deletions). The reviewed worktree resolved under the leased pool slot and
was clean after the mutation check. The code-review graph was populated at the
candidate SHA with 90 nodes, 1241 edges, and 3 Go files. Graph tracing covered
`poolResources`, `ComputeAll`, `PickProvider`, and `ProviderOK`; direct source
inspection also covered CLI quota reporting, router pool selection,
`quotasup.BurnFor`, and standing admission.

Findings: none.

The implementation correctly separates Claude `default` from Fable resources,
keeps the Fable pool exhausted when `fableWeekly` is exhausted, and makes
provider-level `PickProvider`/`ProviderOK` use the metered `default` pool where
one exists. Codex `default`/`spark` behavior and unknown exact-pool handling
remain fail-closed through the existing router and `quotasup` paths.

## Tests run
- `make preflight` — exit 0; boundary, path-leak, merge-policy, and drift checks passed. It emitted only the documented local fence-broker warning.
- `go test ./pkg/usage -run 'Test(PoolResources|ClaudeFableExhaustionDoesNotExhaustDefaultPool|ClaudeDefaultExhaustedRefuses|ComputeAll|PickProvider|ProviderOK|AliasProvider)' -count=1` — exit 0.
- `go test ./cmd/herd -run 'TestStandingQuotaAdmission|TestStanding.*Quota' -count=1` — exit 0.
- `go test ./pkg/quotasup ./pkg/router -count=1` — exit 0.
- Intentional regression mutation restoring the pre-FAC-616 Claude pool classifier, followed by `go test ./pkg/usage -run '^TestClaudeFableExhaustionDoesNotExhaustDefaultPool$' -count=1` — exit 1 as required; the test observed default used=95 instead of 71 and rejected the exhausted default pool.
- Restored the exact candidate `pkg/usage/quota.go`; `git diff --exit-code HEAD -- pkg/usage/quota.go pkg/usage/quota_test.go cmd/herd/launch_policy_test.go` and the same mutation test — exit 0; worktree clean.
- `go test -race ./pkg/usage -run 'Test(PoolResources|ClaudeFableExhaustionDoesNotExhaustDefaultPool|ClaudeDefaultExhaustedRefuses|ComputeAll|PickProvider|ProviderOK)' -count=1` — exit 0.
- `go test -race ./cmd/herd -run '^TestStandingQuotaAdmission' -count=1` — exit 0.
- `go build ./...` — exit 0.
- `go run ./scripts/hermeticity/` — exit 0.
- `go test ./pkg/usage -count=1` — exit 0.
- `git diff --check faae45fecc9edd178794c0e639b4fe27a602a42b 946b006d450bc82e2fba241b235f36f9911eb6e0` — exit 0.

Residual risk: the broader `go test -race ./pkg/usage ./pkg/quotasup ./pkg/router ./cmd/herd -count=1` command was interrupted after 172.165s because an unrelated `cmd/herd` integration test hung while spawning `herd approve FAC-1`; the changed quota and standing-admission race scopes passed independently. Full WSL integration remains coordinator-owned per the packet.

reviewed_at: 2026-09-09T14:51:36Z
