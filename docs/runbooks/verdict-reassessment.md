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

## Recover an acknowledgment after admission

Acknowledgments are retained separately for each exact artifact digest. A new
admitted reassessment does not overwrite the original acknowledgment or its
consumption record. Legacy acknowledgments remain readable.

If admission succeeded but acknowledgment publication failed, retain the reviewer
session and recover using the exact artifact already retained by canonical ingest:

```sh
herd review-ingest --ack-only "$RETAINED_ARTIFACT" --dry-run --json
herd review-ingest --ack-only "$RETAINED_ARTIFACT" --json
```

This path requires byte equality with the retained artifact and the current
SHA/reviewer verdict's recorded artifact digest. It reads the canonical ledger;
it does not append a verdict, repeat tests, change board state, or release a pool
lease. It works after landing without treating an empty diff as a new review.
Changed or superseded evidence, an alternate ledger, and corpus-wide recovery
are refused. The consumer still checks the exact launch identity before retiring
a resident. A successful acknowledgment is not a completion receipt.
