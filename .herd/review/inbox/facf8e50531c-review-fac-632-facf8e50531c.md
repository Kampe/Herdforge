sha: facf8e50531cb1059e1ca3dcef46253a9ef9033e
branch: fac-632-github-status-r2
task: FAC-632
reviewer: assayer
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-head: facf8e50531cb1059e1ca3dcef46253a9ef9033e
---

Reviewed exact SHA facf8e50531cb1059e1ca3dcef46253a9ef9033e on branch fac-632-github-status-r2 (merge-base f2ca0d320043cc57d0262a0c003e898a120e4325 vs origin/main). The leased pool surface was not checked out at the pin when the packet opened; the candidate was detached in this pool worktree and all inspection, tests, and the non-vacuity swap ran there. docs/prompts/review-contract.md is absent on this revision; the review followed .herd/prompts/reviewer.md and .herd/prompts/review-verdict.template.md.

Scope that would merge: pkg/provider/github.go, pkg/provider/github_test.go, pkg/provider/response.go. Production change maps GitHub ListTasks onto the two GitHub issue states (empty/to-do -> open, done/archived/closed -> closed) and fail-closes every other canonical status with errors.Is-compatible ErrUnsupportedStatus before any HTTP call. The r2 tip only empty-terminates the in-review fixture so a missing guard fails on returned-open-issue semantics rather than duplicate-page handling.

No blocking findings.

Evidence:
- githubStateQuery is the sole mapper; ListTasks returns the wrapped error before WithOpDeadline/RetryRead, so unsupported reads do not look like an empty backlog.
- TestGitHubProvider_ListTasks_InReviewUnsupported plus existing open/closed ListTasks tests pass (`go test ./pkg/provider -run 'TestGitHubProvider_ListTasks|TestNormalizeStatus|TestListActiveTasks'` and `-run 'TestGitHub'`).
- Non-vacuity (pool-local, restored immediately): replacing the default branch with `return "open", nil` made TestGitHubProvider_ListTasks_InReviewUnsupported fail as `got tasks=1 err=<nil>`. Restored `pkg/provider/github.go` with `git checkout --`; re-run was green; working tree clean.
- code-review-graph 2.3.7 full build at this SHA (1395 files). detect-changes risk 0.80; githubStateQuery callers are only GitHubProvider.ListTasks. Graph `tests_for githubStateQuery` reported 0 links even though github_test.go covers the path.

Residual risk, not a fail:
- ListActiveTasks fans out ActiveStatuses and still fail-closes on ErrUnsupportedStatus. A scratch GitHub mock showed `cannot list status "in-progress"` with zero tasks. GitHub-backed deps migrate, dispatch active lookup, untargeted `herd review` (in-progress), `herd approve` / wave InReview (in-review) will now error instead of treating open issues as those columns. That matches FAC-632 fail-closed intent; teaching ListActiveTasks to skip unsupported columns would be a separate contract change.
- Unsupported coverage is the in-review default branch only; in-progress/planned share that branch and were not table-tested.
- GitHub UpdateStatus still maps in-review/in-progress writes to open; that write path is unchanged.

PASS: the r2 fixture makes the status-guard regression fail on semantics, the adapter no longer lies about in-review/in-progress on GitHub, and the working tree was left clean at the candidate SHA.
