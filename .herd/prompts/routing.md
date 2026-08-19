# Fleet routing and persistence contract

This is the single mutable routing contract. Every packet and kick template
must reference this file instead of embedding coordinator, supervisor, model,
or quota targets that can change while the fleet is running. Re-read it before
each kick and after a provider or lane-role change.

## Harness delivery

- Claude may use the Stop hook for `/goal`, but must still read durable mail.
- Codex, Grok, OpenCode, AGY, and other non-Stop-hook surfaces treat a settled
  pane as normal: durable mail plus the next Herdr kick is their inbox. Read
  the packet first and do not spend sends re-reporting an empty pane.
- Request rotation before context, turn-count, or provider-warning thresholds;
  hand off exact SHA, lease generation, and next action before quota death.

## Authority and routing

- NEEDS_REVIEW, verdicts, retry findings, and cleanup candidates go only to the
  standing review-harvest supervisor.
- The supervisor sends only exact PASS plus merge-ready evidence to the
  coordinator. The coordinator merges, approves, and closes finished panes.
- No lane sets a board card done. Lanes send evidence; the coordinator projects
  done only after origin/main proves the delivery.
- For peer-to-peer reporting, use `herd send <pane|name> "<text>"` when the
  recipient is live and must see the report in its pane. Use `herd mail send`
  only for durable mailbox delivery, which is not surfaced in the pane; read
  the recipient's durable inbox when using that path.

## Work discipline

- Before claiming, audit origin/main and worktrees. Report shipped evidence or
  a bounded Scope proposal; never rebuild work that cannot be proved missing.
- WIP is one production slice per lane. At three unharvested pins, rebase or
  repair that stack before starting another production slice.
- Empty queue ladder: peek, perform one domain evidence patrol, then park once
  with a durable `parked-until` reason. Do not churn empty reports.
- Failure-path tests must be watched RED against the regression, then GREEN on
  the fix. A test never seen failing is not coverage.
- A repair commit body quotes the exact review finding it addresses. Repeated
  identical findings escalate to the supervisor instead of repinning forever.
