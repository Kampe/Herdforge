sha: f4f9bed4ee37a1a0b88707aa1eae3280c613b88d
branch: refs/herd/review/fac-662-f4f9
task: FAC-662
reviewer: w4
reviewer-family: anthropic
builder-family: unrecorded
verdict: FAIL
reviewed-head: f4f9bed4ee37a1a0b88707aa1eae3280c613b88d
---
Scope: cmd/herd/receipt_recovery.go, cmd/herd/shot_supersession.go. Commit message
claims to address two review findings: (1) `--is-ancestor` independently encoded in
gitpredicates, receipt recovery, and shot supersession; (2) `"candidate supersession:
encode evidence: %w"` independently encoded in shot supersession and lifecycle
supersession.

Finding 1 (--is-ancestor) is fully and correctly fixed. Both call sites in
receipt_recovery.go and shot_supersession.go now call the pre-existing
`commitIsAncestor(root, sha, ref)` helper in cmd/herd/gitpredicates.go instead of
hand-rolling `shotGitOK(ctx, dir, "merge-base", "--is-ancestor", a, b)`. Argument
order is preserved correctly in both replaced call sites (verified by reading both
diff hunks against the old shotGitOK invocations). `go build ./...` succeeds and the
full set of supersession/recovery/ancestor tests pass (`go test ./cmd/herd/... -run
'Ancestor|Supersession|Recovery|Encode'`, all green, no regressions).

Finding 2 (encode-evidence duplication) is NOT actually fixed, despite the commit
message claiming it is addressed. The diff only touches
cmd/herd/shot_supersession.go, renaming its copy from
`"candidate supersession: encode evidence: %w"` to
`"shot: encode candidate supersession facts: %w"`. It does not touch
pkg/lifecycle/supersession.go:102, which still independently hardcodes the original
string `"candidate supersession: encode evidence: %w"` verbatim, and does not
introduce any shared constant/helper either file draws from. The duplication the
review finding flagged is unresolved; the two error sites just now emit two
different strings instead of one shared one, and pkg/lifecycle/supersession.go's
copy doesn't even carry a subsystem prefix consistent with the `lifecycle:`-prefixed
sibling errors already declared a few lines above it in the same file
(ErrCandidateSupersessionState, etc.), which the "shot:" prefix on the touched copy
does follow. Nothing depends on the exact string today (grepped both repos for
"encode evidence"/"encode candidate supersession facts" — no other production or
test code pattern-matches on it), so this is not a functional regression, but the
commit's own stated purpose for finding 2 is not accomplished, and this codebase's
explicit convention (see the FAC-669 comment atop gitpredicates.go: "the next change
... has one place to land") is exactly what this half-fix fails to deliver for
finding 2.

Verdict: FAIL. Either complete the centralization (single shared error text/helper
used by both cmd/herd/shot_supersession.go and pkg/lifecycle/supersession.go), or
correct the commit message to not claim finding 2 is addressed. Finding 1's fix
should be preserved as-is on resubmission -- it is correct and well-verified.
