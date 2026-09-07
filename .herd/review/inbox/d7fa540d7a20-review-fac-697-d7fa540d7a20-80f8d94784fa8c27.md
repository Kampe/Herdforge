sha: d7fa540d7a20b2c695ef7ffc64990af5884c1f19
branch: herd/fac-697
task: FAC-697
reviewer: review-fac-697-d7fa540d7a20
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-head: d7fa540d7a20b2c695ef7ffc64990af5884c1f19
---

Independent xai review of exact SHA d7fa540d7a20b2c695ef7ffc64990af5884c1f19 on herd/fac-697 (merge-base 45330b43773b8b86e80a439cb90a3479232b343e). Isolation: git rev-parse --show-toplevel resolved under .herd/pool/pool-02; the review surface symlink points at that exclusive slot; working tree was clean before and after the read. Risk R2, author family openai, reviewer family xai. Packet named docs/prompts/review-contract.md, which is absent on this candidate; review used .herd/prompts/reviewer.md, review-verdict.template.md, routing.md, and AGENTS.md.

The candidate makes coordinator-published scope part of dispatch and environment-plan identity. Relation-only provenance no longer auto-publishes empty data; it resolves an exact ScopeBinding (repository, provider project, task ref/id, graph revision) from the durable store. Inline scope_packages/scope_files stay content-addressed and auto-published before the launch lease. PutScopeDeclaration is first-writer-wins and idempotent; ReplaceScopeDeclaration is compare-and-swap on the observed scope revision. Envplan Binding now requires ScopeRevision, so a later replace stale-fails admission before ClaimExclusive/worktree/board/launch.

Call sites were updated together: ScopeAuthority.Resolve, durableScopeAdmission.Publish/Acquire/ResolveScope, herd scope publish --task-id, and environmentScopeRevision. Legacy scopefence_scopes rows keyed only by repository/task-ref/graph are dropped on migrate (cannot be safely promoted) and must be republished.

Evidence on this host:
- code-review-graph full build then detect-changes vs origin/main: 13 files, risk 0.85; PutScopeDeclaration callers are publishScopeDeclaration, durableScopeAdmission.Publish, and the new admission tests.
- `go test -count=1 ./pkg/scopefence ./pkg/dispatch ./pkg/envplan ./cmd/herd` PASS (2.970s / 21.560s / 0.682s / 117.506s)
- Non-vacuity in the pool tree only, then restored: mutant in PutScopeDeclaration that returned the existing row on content conflict turned TestScopePublicationConcurrentWinnerAndIdempotentRetry RED (`won=2, want exactly one`). Restored candidate re-run GREEN. git status --porcelain empty; HEAD still d7fa540d7a20b2c695ef7ffc64990af5884c1f19.

Residual risk, not blocking: ScopeFromProvenance still treats hold paths as inline scope; envplan unit fixtures always include ScopeRevision so validate()'s new empty-field check is covered mainly by dispatch integration (TestProductionDispatchRejectsScopeChangedAfterEnvironmentPlan); graph file-count expectation is rebound from the stored snapshot at Acquire by design.

No blocking findings remain for this exact revision.
