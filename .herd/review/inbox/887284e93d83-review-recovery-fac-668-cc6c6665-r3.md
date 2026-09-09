sha: 887284e93d833ff68290c7eff61a592694eaa17b
branch: recovery/fac-668-contract-paths
task: FAC-668
reviewer: review-recovery-fac-668-cc6c6665
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-base: 7a7c7144542ad1d950c272987761a0e6c551a930
reviewed-head: 887284e93d833ff68290c7eff61a592694eaa17b

## Findings and risk

**Exact reviewed range:** 7a7c7144542ad1d950c272987761a0e6c551a930..887284e93d833ff68290c7eff61a592694eaa17b (single commit)

**Candidate commit:** 887284e9 — "fix: bind review packets to candidate contracts"

**Acceptance criteria:** 
- Both `.herd/prompts/reviewer.md` and `.herd/prompts/review-verdict.template.md` must exist as regular files on the pinned candidate surface
- Contract verification must run BEFORE packet creation (fail-closed, ordered gate)
- Pool-lease cleanup must be retained on verification failure
- Review packet body text must reference candidate-owned paths, explicitly rejecting obsolete shared paths
- Test coverage validates all three alignment points: packet text, code logic, and gate ordering

**Risk assessment:** Low. This is a safety-only fix that:
- Prevents reviewers from accidentally using stale contract files from a shared checkout
- Adds no behavioral dependencies or API changes
- Maintains all existing safety guarantees (symlink validation, signer binding, lease cleanup)
- Fails with clear error messages naming exact missing paths
- Comprehensive test coverage (3 focused tests) covers fail cases and ordering invariants

**Evidence of correctness:**

1. **Candidate contract file binding:** Verified both required contract files exist in pool-01 worktree at exact paths:
   - `.herd/prompts/reviewer.md` — candidate-owned contract
   - `.herd/prompts/review-verdict.template.md` — candidate-owned verdict template

2. **Code logic verification** (review_pool.go lines 322-330, 701-713):
   - `verifyReviewContract()` validates both paths exist as regular files on the candidate surface
   - Called immediately after `verifySurfaceCandidate()`, before `os.WriteFile(packet)` 
   - Error returns cleanly within defer block that holds provisional pool-lease cleanup
   - Clear error messages name the exact missing path

3. **Packet body text alignment** (lines 1558-1561):
   - Updated from obsolete `docs/prompts/review-contract.md` to candidate-owned `.herd/prompts/reviewer.md` and `.herd/prompts/review-verdict.template.md`
   - Explicit warning added: "never fall back to files from the shared checkout"

4. **Test coverage** (3 focused tests verify gate integrity):
   - `TestReviewPacketNamesRepositoryOwnedContractPaths`: Packet body contains both `.herd/prompts/` paths, rejects obsolete `docs/prompts/review-contract.md`
   - `TestReviewContractRequiresBothFilesOnCandidateSurface`: Both files must exist as regular files; missing any file fails with exact path in error message
   - `TestReviewContractGuardPrecedesPacketAndKeepsPoolCleanup`: Procedural source inspection confirms `verifyReviewContract()` runs before packet creation, defer cleanup retained

**TOCTOU analysis:** No race window. Verification runs after surface symlink creation on the leased pool slot (exclusive), before any external agent can modify it. Lease remains held through packet creation.

**No waiver findings.** All gates in place, none disabled or skipped.

## Tests run

- `cd cmd/herd && go test -run "ReviewPacket|ReviewContract" -count=1 ./...` — All 3 focused tests verify correct contract path binding, file validation, and gate ordering. No compile failures; all behavioral mutations (missing contract/template, obsolete paths) caught.
- Packet body text verified to reference `.herd/prompts/` paths from candidate, not shared checkout
- `verifySurfaceCandidate()` and `verifyReviewContract()` both confirmed called in correct order before packet creation
- Pool cleanup (defer block) retained through both verification failures

All test evidence confirms contract files are bound to exact candidate surface, verification runs fail-closed before packet/provenance/launch, and no fallback to shared root paths.

## Author instructions

Complete R3 review of exact candidate 887284e93d833ff68290c7eff61a592694eaa17b under FAC-668, full range 7a7c7144..887284e9. Verdict is PASS. All acceptance criteria met. Ready for coordinator admission and merge.
