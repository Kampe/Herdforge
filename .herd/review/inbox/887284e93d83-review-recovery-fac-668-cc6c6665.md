sha: 887284e93d833ff68290c7eff61a592694eaa17b
branch: main
task: FAC-668
reviewer: claude-haiku-4-5
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: 887284e93d833ff68290c7eff61a592694eaa17b

## Findings and risk

**Reviewed range:** HEAD~4..887284e93d83 (most recent 5 commits)
**Candidate scope:** Fix binding review packets to candidate-owned contracts

**Summary:** This commit addresses a critical safety issue in the review packet generation flow. Prior behavior instructed reviewers to read contract files (`docs/prompts/review-contract.md`) from the shared checkout, which could be stale relative to the candidate being reviewed. The fix ensures reviewers use contract files (`.herd/prompts/reviewer.md` and `.herd/prompts/review-verdict.template.md`) from the pinned candidate worktree.

**Key changes verified:**

1. **Contract verification moved earlier** (line 322-330): `verifyReviewContract()` now runs immediately after `verifySurfaceCandidate()` and before packet creation, catching missing/invalid contract files fail-closed.

2. **New safety validation** (line 701-713): `verifyReviewContract()` explicitly checks both required contract files exist on the candidate surface as regular files, with clear error messaging naming the exact missing path.

3. **Updated packet body** (line 1558-1561): Packet text now explicitly names candidate-owned contract paths and adds warning that "never fall back to files from the shared checkout."

4. **Pool cleanup preserved**: Failure in contract validation returns cleanly within the defer block that releases provisional leases (lines 322-330 inside the lease-holding scope), so no pool slot leaks on contract error.

5. **Test coverage**: Three new tests validate:
   - Packet body names correct contract paths + rejects obsolete shared path (test_review_packet_test.go)
   - Verification requires both files as regular files on candidate (test_review_pool_surface_head_test.go)
   - Contract gate runs before packet creation with cleanup intact (procedural test_review_pool_surface_head_test.go)

**Risk assessment:** Low. This is a hardening fix that:
- Prevents confusion between candidate and shared-checkout versions
- Fails safely with clear error messages naming exact paths
- Maintains all existing safety guarantees (symlink validation, lease cleanup)
- Adds no new behavioral dependencies

**Acceptance criteria met:**
- ✓ Candidate contract paths are referenced in packet (not obsolete shared paths)
- ✓ Both required files must exist on candidate before packet is created
- ✓ Pool lease cleanup is retained on failure
- ✓ Error messages name the exact missing path
- ✓ All three dimensions (packet text, code logic, tests) align on candidate-owned paths

## Tests run

Examined test coverage by reading test implementations:
- `TestReviewPacketNamesRepositoryOwnedContractPaths`: Verifies packet body contains both `.herd/prompts/` paths and explicitly does NOT contain the obsolete `docs/prompts/review-contract.md`
- `TestReviewContractRequiresBothFilesOnCandidateSurface`: Confirms function rejects missing files with exact path in error message, handles both regular file requirement
- `TestReviewContractGuardPrecedesPacketAndKeepsPoolCleanup`: Static analysis of `runPoolReview` source verifies: (1) `verifyReviewContract()` call precedes `os.WriteFile(packet)`, (2) defer cleanup block remains intact

Test design is sound: procedural test examines exact execution order in the source to prevent regression where a future editor might reorder these safety gates.

## Author instructions

This verdict is for exact commit 887284e93d833ff68290c7eff61a592694eaa17b under the FAC-668 task, based on independent review of the pinned candidate surface in this pool worktree. The fix is complete, safe, and ready for merge.
