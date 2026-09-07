# Recover an incomplete admitted record

Use this only when an exact candidate already has an admitted independent PASS
and a retained artifact, but an earlier pool record omitted its proven builder
family or risk tier. This operation completes the record; it neither changes the
verdict nor grants merge or board-completion authority.

Run from a worktree of the same project with the canonical ledger selected:

```zsh
export HERD_REVIEW_LEDGER="$HERD_PROJECT_ROOT/.herd/review-ledger.jsonl"
herd review-complete-record FAC-NNN --candidate FULL_SHA \
  --reviewer ORIGINAL_REVIEWER --artifact RETAINED_ARTIFACT
```

The command requires the exact retained bytes recorded in the current verdict,
an explicit reviewed base/head and task, a real verification digest, and a native
launch receipt predating and reaching the candidate. It computes the existing
risk classifier's tier from that immutable range. There are no family or tier
assertion flags, and no corpus mode.

A successful call appends one record while preserving the existing lease, patch,
and other recorded fields. Conflicting nonempty bindings or review dissent are
refused. Repeating the same call writes nothing. A failure is not evidence that
an absent family or dependency list is empty or safe.

Inspect the canonical record and native receipt admission afterward. Only the
normal landing-proof, receipt and approval workflow can complete the task. Do
not edit historical rows, rename the reviewer, change retained evidence, or use
this command to substitute a package test digest for a full-suite receipt.
