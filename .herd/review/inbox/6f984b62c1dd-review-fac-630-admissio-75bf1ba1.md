sha: 6f984b62c1dd5bab225003c197df119f8f5ac75c
branch: fac-630-admission-diag
task: FAC-630
reviewer: pool-02
reviewer-family: openai
builder-family: anthropic
verdict: FAIL
reviewed-head: 6f984b62c1dd5bab225003c197df119f8f5ac75c
---

Blocking finding: `pkg/reviewledger/admission_diagnostic_test.go:135` is vacuous against the exact pre-FAC-630 regression. `TestRefusalNamesANonIndependentReviewerFamily` accepts any reason containing `"independent"`, but the old generic refusal was `"no independent PASS verdict with durable verification evidence"`; therefore the test passes without the new discriminating same-family diagnostic.

Non-vacuity evidence: I temporarily restored `pkg/reviewledger/admission.go` to the exact pre-FAC-630 blob at `HEAD^^` (confirmed with `git diff --exit-code HEAD^^ -- pkg/reviewledger/admission.go`) and ran `go test ./pkg/reviewledger -run 'TestRefusalNames|TestRefusalDistinguishes' -count=1 -v`. Four new tests failed as intended, while `TestRefusalNamesANonIndependentReviewerFamily` passed. The source was then restored exactly; `git diff --exit-code HEAD -- pkg/reviewledger/admission.go` passed and the worktree was clean. Make this assertion require a diagnostic-specific fragment absent from the generic reason, such as `"equals the builder family"`.

Validation at the reviewed head: `go test ./pkg/reviewledger -count=1` passed. `make lint all` did not complete because `scripts/security-gate.zsh` failed with `jq: Cannot iterate over null`. Running `make all` separately passed preflight, build, nested contract tests, and hermetic compilation, but the shuffled full suite failed in `pkg/herdr`, `pkg/review`, `pkg/sync`, and `pkg/verifier`; none of those packages is changed by this test-only candidate, but the repository-wide gate was not green.

Review limitations: `docs/prompts/review-contract.md` is absent from the candidate tree, and the required `code-review-graph` CLI plus repository wrapper are unavailable in this leased pool, so review used the exact candidate diff, focused source inspection, and direct tests.
