# Reconcile an attested reconstruction

An original PASS remains attached to the reviewed SHA. Reconstructed content
must have a retained reconstruction event, an exact base, and mechanical
content equivalence. Code blobs must be identical. Markdown may relocate the
same additions/deletions without changing them. Different changed paths,
missing attestations, mismatched digests, or changed code are refused.

Read the existing attestation without changing it:

```sh
herd review-ledger reconstruction-digest "$REVIEWED_SHA" "$CONTENT_SHA"
```

Inspect the returned proof and select its exact digest. Do not create a new
attestation merely to satisfy this command. Then, from the canonical control
context with the appropriate review ledger, reconcile the already-landed work:

```sh
herd harvest-merge "$LANE" --branch "$BRANCH" --verify-landed \
  --ref "$REF" --candidate "$REVIEWED_SHA" --base-sha "$REVIEWED_BASE" \
  --reconstructed-from "$CONTENT_SHA" --reconstruction-base "$CONTENT_BASE" \
  --reconstruction-digest "$ATTESTATION_DIGEST" --pr "$PR"
```

This command retains the existing verify-landed behavior, including worktree
rebase/landing observation. Use a dedicated, clean landing worktree. It is not
a read-only dry run. All reduced admission gates still apply, including an
independent PASS, verification digest, and recorded risk tier. An old verdict
without a risk tier remains refused; this repair does not invent one.

Only after a sealed receipt is returned and independently read back should
the coordinator run receipt-backed `herd approve "$REF"` and verify provider
Done. The receipt binds the original reviewed SHA, reconstructed SHA and base,
attestation digest, and actual equivalent landed commit. Ordinary reconciliation
without reconstruction flags keeps its previous behavior.
