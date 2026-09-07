sha: <the exact 40-hex candidate commit this verdict is about>
branch: <the candidate branch from the review packet>
task: <the exact board card ref from the review packet>
reviewer: <your authenticated reviewer lane name, never a coordinator>
reviewer-family: <your actual model family, not the harness brand>
builder-family: <the author family proven by the admitted review packet>
verdict: PASS | FAIL | BLOCKED
reviewed-base: <the exact 40-hex diff base you independently reviewed>
reviewed-head: <git rev-parse HEAD in the tree you actually read; must equal sha>
retry-of: <optional prior reviewer lane for an exact-head retry; otherwise omit this line>
reassesses: <optional prior verdict-event digest for authenticated reassessment; otherwise omit this line>
---
## Findings and risk

{{review-evidence}}

## Tests run

{{executed-verification}}

## Author instructions

Replace every placeholder with your own evidence; remove unused optional header
lines. The template itself is not a verdict or verification evidence. Keep all
headers together before `---`: no title, routing sentence or other prose may
interrupt the leading metadata block. A single opening `---` is also accepted.

In Findings and risk, record the exact reviewed range, risk floor from
`herd review-classify`, acceptance criteria, numbered findings and residual
risks. Do not lower the classifier floor. For R3, explicitly assess the required
high-risk gates against concrete evidence. Supply at least 200 non-whitespace
characters of independent reasoning, including for FAIL and BLOCKED.

In Tests run, replace the placeholder with commands you actually executed and
their observed outcomes, failures, limitations and any RED/GREEN mutation proof.
If you executed no checks, leave that section empty and state the limitation in
Findings and risk; do not manufacture a digest or count these instructions as
verification. The parser also recognizes Verification, Verification evidence,
and Commands run. Headings such as Tests or Tests & Invariants are not aliases.
Package checks do not establish full-suite coverage. A builder or CI full-suite
receipt is separate evidence: identify its exact candidate and scope outside
Tests run, rather than representing its commands as your own execution.

## Identity and optional metadata

Use `task:` for the exact card this review is about. A mention of a sibling card
in the body is not an explicit task binding. Parser compatibility aliases are
`task-id`, `card`, and `ticket`; prefer the canonical `task` spelling.

Record `reviewed-base` from the diff you actually assessed and `reviewed-head`
from the pinned worktree. Do not substitute a candidate's parent for a reviewed
base you did not inspect. A stated head mismatch is refused. Legacy artifacts
may omit these fields at some gates; new completion-ready reviews need truthful
exact bindings.

Model family and harness name are different concepts: an AGY session can run an
Anthropic model. Use authenticated model provenance. Never invent a builder
family or override the packet's proven family; ask the supervisor to resolve a
missing or contradictory binding. Different-family review is required for
R1–R3, and a coordinator cannot review its own work at any tier.

`retry-of` names a prior reviewer lane for an exact-head retry. It is not
permission to replace a verdict. Use `reassesses` only for a supported,
authenticated same-reviewer reassessment of the same task and SHA, with new
artifact and verification evidence, binding the exact prior verdict-event
digest supplied by the review process. It is not the candidate SHA or the
artifact file digest. Never rename a reviewer, rewrite historical evidence, or
invent the prior digest to evade duplicate protection. Supervisor admission
still validates the reassessment and appends it; the header alone grants no
merge authority.

For coordinator retirement artifacts only, use `verdict: RETIRED` and
`authority: <coordinator name>` (`asserting-authority` is a compatibility alias).
RETIRED is not an independent review verdict or PASS.

The parser accepts the canonical headers above and the documented aliases.
It also accepts a small advisory set: `merge-recommendation`, `recommendation`,
`confidence`, `skills-used`, `model-family`, `provider`, and `model`. Those are
ignored for admission and cannot supply provenance or risk-tier authority.
Unrecognized headers and misspelled gate keys are refused. Do not duplicate a
key with conflicting values. Put other observations in the body. A value may
contain a colon; only the first colon separates its header key from its value.

## Delivery and retention

Read `.herd/prompts/routing.md` before delivery. Write the reviewer-authored
artifact to the exact inbox path assigned by the supervisor; a pane message or
provider comment is not an ingested verdict. Send the artifact path and report
using `herd send <agent> --file <path>`, or the routing contract's durable mail
fallback when pane delivery is unavailable. Do not send directly to the
coordinator or use repository `bin/herd-*` scripts.

The supervisor validates and ingests the exact artifact and checks retained
identity, risk and verification bindings before recommending retirement. Keep
the author session resumable until required handoff evidence is retained and
validated. Only the supervisor's merge-ready handoff goes to the coordinator.
