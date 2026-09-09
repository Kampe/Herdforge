# FAC-773 completion wake

The coordinator-side safe-boundary consumer is the native Herdforge CLI. When
PATH has an older `$HOME/.local/bin/herd` Herdr-only wrapper, invoke the source
entry point explicitly (or first verify that the installed `herd` is this
Herdforge build):

```zsh
go run ./cmd/herd watch --wake --recipient <exact-herdr-name> --workspace <exact-workspace-id> \
  --stream --interval 5 --timeout 14400
```

Run it from the coordinator repository checkout. The command resolves the
repository's canonical `.herd/control-mail.jsonl` (or the repo-relative
`HERD_MAIL_FILE`) and rechecks the exact Herdr recipient before every
envelope. It surfaces ordinary report mail and queued routine mail only when
the recipient is `idle` or `done`; `working`, `starting`, and unknown status
make zero prompt/key/signal calls. Authenticated control and callback
envelopes remain on their native consumers.

Producers may run from a linked worktree: leave `HERD_PROJECT_ROOT` unset and
allow the existing Git common-directory authority to resolve the project. A
lane-specific `HERD_ROOT` is not a project-mail anchor. If a launcher supplies
`HERD_PROJECT_ROOT`, it must name the exact canonical project root; do not
replace it with a guessed worktree basename or fall back to a foreign repo.

Incident evidence confirms the misplaced WSL report was caused by an explicit
cwd-relative override, not by canonical-root resolution: the retained P7
Codex session invoked `go run ./cmd/herd mail send ... --mail
.herd/control-mail.jsonl` while its working directory was the linked worktree
`.worktrees/mender-fac781-route`. That correctly wrote the envelope to that
worktree's mailbox even though `HERD_PROJECT_ROOT` was already the canonical
repository. When reporting to the coordinator, omit `--mail` so native
canonical resolution applies, or pass a canonical-root-qualified path. Never
use cwd-relative `.herd/control-mail.jsonl` from a linked worktree. The generic
explicit-project-root rejection experiment was reverted and is not part of
this contract. The old WSL PATH wrapper was separately backed up and replaced
with the relative `../../Projects/Herdforge/bin/herd` symlink; verify its
version and native `mail --help` before use.

Delivery states are distinct: mailbox append is `queued`, prompt submission is
`submitted`, pane/task evidence is `consumed`, and the handled sidecar is
`handled`. Pending state is retained until consumption proof and handled-state
write both succeed. A crash after prompt consumption but before the handled
write can redeliver once after restart (at-least-once semantics); the stable
envelope ID prevents duplicate acknowledgement and the recipient must be
re-read before another message.

The mailbox is local to the coordinator host. A WSL-only write is not Mac
transport. For a WSL source, configure the existing supported repo-relative
`HERD_MAIL_FILE` to a coordinator-visible mailbox populated by the separately
approved transport, then run the command above on Mac. If that source or
transport is unavailable, the envelope remains pending and the wake command
fails visibly; it does not invent an SSH alias, host receipt, or read another
project's mailbox. Review artifacts continue through `herd verdict-push`.

## Explicit WSL ordinary-mail relay

The supported ordinary-report bridge is the operator-invoked relay script,
using the native `mail pending`, `mail import`, `watch --wake`, `mail status`,
and `mail ack` operations. `herdr-deliver` is not this bridge: it submits
directly and has no durable working-pane refusal/retry identity.

Run one relay/watch owner per exact recipient, with the configured host and
canonical mailbox paths supplied explicitly:

```zsh
: "${HERD_WSL_HERD_ROOT:?set the absolute canonical WSL Herdforge root}"
: "${HERD_CANONICAL_ROOT:?set the absolute canonical coordinator Herdforge root}"

./scripts/fac773-mail-relay.zsh \
  --host wsl-box \
  --remote-binary "$HERD_WSL_HERD_ROOT/bin/herd" \
  --remote-mail "$HERD_WSL_HERD_ROOT/.herd/control-mail.jsonl" \
  --local-binary "$HERD_CANONICAL_ROOT/bin/herd" \
  --local-mail "$HERD_CANONICAL_ROOT/.herd/control-mail.jsonl" \
  --recipient <exact-herdr-name> --workspace <exact-workspace-id>
```

Both root variables must expand to absolute canonical roots before invocation;
the relay still rejects relative paths. `wsl-box` is the configured source
host, not a guessed SSH alias, and the recipient/workspace remain exact.

The relay uses `ssh` with `BatchMode`, bounded connection/command deadlines,
and the literal supported host `wsl-box`; it rejects unsafe or relative path
arguments. Remote JSON is collected as ordinary pending mail, imported with an
identity derived from `source-host + source-envelope-id`, and appended through
`Mailbox.AppendEnvelopeContext`. The source ID is informational metadata, not
authentication. Exact duplicate imports are no-ops; changed bytes, recipient,
sender, subject, control/callback class, or unknown privileged fields fail
closed. Message bodies never enter an SSH command argument or shell evaluation.

The relay invokes the existing safe-boundary watcher and checks local
`mail status` before issuing remote `mail ack` for the original source ID.
Busy/unknown recipients make zero pane writes and leave both sides pending.
A crash or failed remote ack after local handling is at-least-once: the next
run imports idempotently, observes the existing local handled sidecar, and
retries only the remote acknowledgement. An unavailable or unreadable remote
mailbox is a visible failure. This relay transports ordinary progress only;
it cannot admit privileged control, reviews, or authenticated reviewer-host
claims.
