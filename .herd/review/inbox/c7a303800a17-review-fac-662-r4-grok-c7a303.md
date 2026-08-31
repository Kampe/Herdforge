sha: c7a303800a17387eb5cc1ab1e65ebc10f05da1cf
branch: herd/fac-662
task: FAC-662
reviewer: review-fac-662-r4-grok-c7a303
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-base: 9117a649319b949b422d6280711d9d8f914f57b8
reviewed-head: c7a303800a17387eb5cc1ab1e65ebc10f05da1cf
---

# FAC-662 r4 review (c7a3038)

No blocking findings. Cross-family independence holds: builder-family `openai` is taken from the canonical pre-edit launch receipt for `FAC-662-repair3` (created `2026-08-31T18:44:35Z`, candidate commit `2026-08-31T19:37:40Z`); reviewer-family is `xai`.

Isolation: `git rev-parse --show-toplevel` = leased pool-01; `HEAD` = `c7a303800a17387eb5cc1ab1e65ebc10f05da1cf`; `git status --porcelain` empty before and after verification. `docs/prompts/review-contract.md` is absent on this candidate; the tracked contracts used were `.herd/prompts/reviewer.md` and `.herd/prompts/routing.md`.

Inspected the entire immutable range `9117a649319b949b422d6280711d9d8f914f57b8..c7a303800a17387eb5cc1ab1e65ebc10f05da1cf` (4 commits, 12 files, +1899/-16):

- `3a83b7d19` feat: support candidate supersession recovery
- `fe954032e` fix: separate recovery authority modes
- `c6173ef5f` fix: centralize candidate supersession rules
- `c7a303800` fix(lifecycle): centralize supersession encoding errors

## Findings

No findings.

The prior FAIL on `f4f9bed4ee37` is repaired: `--is-ancestor` has a single owner in `commitIsAncestor`, and JSON encoding plus its failure contract now live in `lifecycle.EncodeCandidateSupersessionEvidence` / `ErrCandidateSupersessionEncoding`. Both production call sites (`Machine.SupersedeCandidate` and `supersedeShotLifecycleCandidate`) use that helper; `cmd/herd` no longer wraps a second encode-error string.

## Authority, identity, CAS, readback

- Generic `herd receipt issue --role recovery` remains an unscoped sentinel. Explicit `--candidate-supersession` is recovery-only; verifier+flag is refused and does not mutate the current receipt (`TestReceiptIssueCLI_*`).
- Scoped issuance authenticates the prior signed worker/recovery receipt, refuses a generic sentinel as supersession provenance, preserves the authenticated base when origin/main has advanced, and binds the replacement to exact HEAD, registered contained worktree, reachability (`commitIsAncestor` base⊆candidate⊆branch), and a clean tree.
- Shot-side validation rejects worker authority without a live launch, rejects worker+scope, and accepts closed-worker recovery only with scope `candidate-supersession`, lease `task:recovery`, and authorized SHA equal to the replacement.
- Lifecycle supersession is Recovering→Recovering with exact CAS on task/repo/seq/lease/branch/old SHA, refuses active integration and terminal states, and mandatory readback checks state+event identity/payload. Winner retry is idempotent; concurrent distinct replacements have one winner.
- Prior receipts/reviews/approvals remain SHA-bound to the old candidate; replacement inherits no passing evidence and no merge readiness.

## Partial-write and history

- Event insert + state CAS share one SQLite transaction; an injected state-write failure rolls back and leaves seq/candidate unchanged.
- `persistRecoveryReceipt` writes local then canonical; a post-canonical failure removes only the exact new session and restores the authenticated prior local context.
- Append-only history keeps the old SHA on the prior event and records the replacement edge in the new payload (`old_candidate_sha` / `new_candidate_sha` / `prior_lifecycle_sequence`).

## Four-file encoding-error repair

`cmd/herd/shot_supersession.go`, `cmd/herd/shot_supersession_test.go`, `pkg/lifecycle/supersession.go`, `pkg/lifecycle/supersession_test.go`. Shared helper, no import cycle. AST guard on the adapter fails closed if an independent encode/marshal/serialize+supersession string returns.

Non-vacuity: temporarily restored `return fmt.Errorf("shot: encode candidate supersession facts: %w", err)` in `cmd/herd/shot_supersession.go`. `TestShotCandidateSupersessionEncodingHasOneLifecycleOwner` failed with that exact independent message. Restored via `git checkout -- cmd/herd/shot_supersession.go`; HEAD unchanged and porcelain empty.

## Residual risk (non-blocking)

- The lifecycle `SupersedeCandidate` call site is not itself AST-scanned; re-inlining `json.Marshal` there while keeping the helper would not trip the cmd/herd guard. Production still has one owner.
- `collectShotSupersessionFacts` uses process `Getwd()`, which is correct for a live worktree callback; package tests that invoke the unstubbed Recovering path therefore inspect the test process cwd rather than the temp root and fail closed on missing authority.

code-review-graph 2.3.7 full build at this SHA: 1406 files, 16683 nodes. Callers of `EncodeCandidateSupersessionEvidence`: `SupersedeCandidate`, `supersedeShotLifecycleCandidate`, plus the owner tests. Detect-changes risk 0.85 on 12 files; reported "untested" names for `runReceiptIssue`/`authenticatedRecoveryIdentity`/`persistRecoveryReceipt` are graph-edge gaps, contradicted by the CLI and unit tests below.

## Verification

Go toolchain: `/home/kampe/.local/share/mise/shims/go version` → `go version go1.26.4 linux/amd64` (exit 0).

1. `/home/kampe/.local/share/mise/shims/go test ./pkg/lifecycle/ -count=1 -timeout 120s -run 'Supersession|Encode'` → exit 0 (`ok pkg/lifecycle`). Covered encode owner, history/idempotency, exact fences, non-recovering/integration, SHA-bound evidence, partial-write rollback, readback mismatches, terminal states, concurrent winner.
2. `/home/kampe/.local/share/mise/shims/go test ./pkg/dispatch/ -count=1 -timeout 120s -run 'CandidateSupersession|AuthorityScope|TaskContextValidate'` → exit 0. Includes `TestTaskContextValidate_CandidateSupersessionScopeIsExplicitAndRecoveryOnly`.
3. `/home/kampe/.local/share/mise/shims/go test ./cmd/herd/ -count=1 -timeout 300s -run 'Supersession|Recovery|Encode|ReceiptIssueCLI|LifecycleLease|Ancestor'` → exit 0. Includes recovery identity/base preservation, persistence compensation, generic-sentinel refusal, receipt-issue CLI generic vs scoped, Recovering path wiring, encoding owner AST test, and authority/Git mismatch table.
4. `/home/kampe/.local/share/mise/shims/go test ./cmd/herd/ -count=1 -timeout 120s -run 'ExactShotLaunchReceipt|ShotRegisteredWorktree|commitIsAncestor|IsAncestor'` → exit 0 (launch provenance + worktree containment/escape tests).
5. `/home/kampe/.local/share/mise/shims/go test ./pkg/lifecycle/ -race -count=1 -timeout 180s -run 'Supersession|Encode'` → exit 0.
6. `/home/kampe/.local/share/mise/shims/go build -o /dev/null ./cmd/herd/` → exit 0.
7. `/home/kampe/.local/share/mise/shims/go run ./scripts/hermeticity/` → exit 0.
8. Encoding mutation proof: mutant test exit 1 (RED); restore `git checkout -- cmd/herd/shot_supersession.go`; `git status --porcelain` empty; `HEAD` still `c7a303800a17387eb5cc1ab1e65ebc10f05da1cf`.
