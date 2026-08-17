sha: <the 40-hex commit id this verdict is ABOUT>
branch: <the branch the candidate lives on>
reviewer: <your lane name — never a coordinator>
reviewer-family: <your model family: anthropic|openai|google|xai|moonshot|...>
builder-family: <the AUTHOR's family — must differ from yours>
verdict: PASS | FAIL | BLOCKED
reviewed-head: <output of `git rev-parse HEAD` in the tree you actually read>
---
Your evidence goes here, below the `---`.

Delivery: send this verdict and its findings to the standing review supervisor. The coordinator receives only the supervisor's merge-ready PASS handoff.

Use the Herdforge/Herdr delivery path (`herdr agent prompt` or
`herd herdr-deliver --file`). Do not send verdicts through repository
`bin/herd-*` scripts or directly to the coordinator. The supervisor owns the
exact-SHA ledger row, retry loop, author feedback, and reviewer-tab cleanup.

# The seven keys above are the COMPLETE accepted set

`herd review-ingest` refuses an artifact carrying any other key. That is
deliberate: a misspelled `reviewed-head` silently disables the gate that catches
a reviewer grading the wrong tree, and a key nothing reads surfaces nothing at
all. If you need to record something else, put it in this body.

## Rules the gate enforces, and why each exists

- **Front matter is the LEADING block.** No title, no prose above it. A line
  like `Reviewer: see the assignment below` lowercases to a real key and would
  otherwise shadow the honest header beneath it.
- **No key twice with different values.** Neither first-wins nor last-wins is
  safe — one admits `verdict: FAIL` followed by `verdict: PASS`, the other
  admits the shadowing above. Ambiguity is refused rather than resolved.
- **A coordinator may never be the reviewer.** Self-verification does not
  qualify at any risk tier.
- **reviewer-family must differ from builder-family.** Same family is not an
  independent read.
- **reviewed-head must equal sha.** It is provenance, not proof — you could
  write anything. But you review from a disposable worktree checked out AT the
  pin, so a truthful reviewer reports it without effort and a wandering one has
  to state a mismatch or lie outright. Omitting it is tolerated for older
  reviewers; a stated mismatch never is.
- **The body must carry at least 200 characters of non-whitespace evidence.**
  A verdict with no reasoning is not a review. This applies to FAIL and BLOCKED
  too: a bare rejection is unactionable.

A value may contain a colon (`branch: feat/thing:sub` parses correctly) — only
the first colon on a line separates key from value.
