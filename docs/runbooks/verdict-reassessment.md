# Reassess new evidence on the same candidate

A repeated SHA/reviewer verdict remains a duplicate by default. Do not rename
the reviewer or create a new commit merely to avoid this rule.

Read the current exact verdict binding:

```sh
herd review-ledger verdict-digest "$SHA" "$REVIEWER"
```

The original authenticated reviewer must retain a new review artifact with
unchanged SHA, reviewer, task and family identities, new verification evidence,
and this explicit leading YAML field:

```yaml
reassesses: <digest returned for the prior verdict event>
```

Use the ordinary supported ingestion flow on that exact artifact:

```sh
herd review-ingest "$ARTIFACT" --dry-run --json
herd review-ingest "$ARTIFACT" --json
herd review-ledger readiness "$SHA"
```

Ingestion hashes the actual artifact bytes; the reviewer cannot supply that
hash. The verification section must yield a nonempty digest different from
the prior verdict. Both the dry run and locked append validate the bound prior
event. A stale digest refuses; exact replay is a no-op. New rows retain the
prior event digest and artifact digest without rewriting history. Reassessment
cannot also declare `retry-of` to clear another reviewer. Another reviewer's
FAIL continues to veto, and a later authenticated FAIL can replace this
reviewer's PASS under the same explicit process.

All existing provenance, exact candidate, artifact and admission checks remain
in force. Admission is not permission to merge or mark the card Done.
