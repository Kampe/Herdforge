# FAC-773 completion wake

The coordinator-side safe-boundary consumer is:

```zsh
herd watch --wake --recipient <exact-herdr-name> --workspace <exact-workspace-id> \
  --stream --interval 5 --timeout 14400
```

Run it from the coordinator repository checkout. The command resolves the
repository's canonical `.herd/control-mail.jsonl` (or the repo-relative
`HERD_MAIL_FILE`) and rechecks the exact Herdr recipient before every
envelope. It surfaces ordinary report mail and queued routine mail only when
the recipient is `idle` or `done`; `working`, `starting`, and unknown status
make zero prompt/key/signal calls. Authenticated control and callback
envelopes remain on their native consumers.

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
