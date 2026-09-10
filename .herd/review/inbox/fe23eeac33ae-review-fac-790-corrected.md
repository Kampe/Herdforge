sha: fe23eeac33ae87c467cf84fe67c0c277e86a72a6
branch: harvest/provider-state-1414
task: FAC-790
reviewer: review-fac-790-fe23eeac33ae
reviewer-family: anthropic
builder-family: google
verdict: PASS
reviewed-base: 61d9e868a8533a6c70eff09024d7ad25bc0f9a27
reviewed-head: fe23eeac33ae87c467cf84fe67c0c277e86a72a6
---

## Findings and risk

### Integration assessment: Two-file merge resolution (a63 → fe23)

**Scope:** Google-authored integration of a63 Anthropic source into fe23 merge commit, focusing on two-file merge resolution: `pkg/attention/attention.go` and `pkg/attention/attention_test.go`. Compatibility verified against broker agentDecision and TriageWithEvidence contract.

**Merge-parent resolution analysis:**

1. **Decision field addition to Item struct**: Lines 65-72 add `Decision *broker.Decision` JSON field with documentation note. No loss of existing PaneID, Held, HeldReason fields. Struct remains backward-compatible for JSON serialization.

2. **agentDecision helper (lines 220-239)**: New function introduced AFTER ClassifyAgentWithEvidence body completes. Preserves all original ClassifyAgentWithEvidence logic without semantic change. Constructs broker.Decision based on agent status:
   - "working"/"starting" → broker.Unknown with ClassBuild progress
   - "idle"/"done"/"blocked" → broker.Decision with ClassWait progress  
   - default → broker.Unknown with ClassProbe progress
   - All cases preserve progress.Record context

3. **ClassifyAgentWithEvidence integration (line 179)**: Single line added: `item.Decision = agentDecision(name, status)` after item struct initialization. Original eval logic at lines 182-218 unchanged. Return statement passes item with Decision field intact.

4. **TriageWithEvidence preservation**: Function signature unchanged. Calls ClassifyAgentWithEvidence at line 292 with all original parameters. Returns item containing Decision field through broker classification contract.

5. **Test compatibility**: New test `TestClassifyProgressEventWaitIsNotUsefulWork` validates progress classification. Original `TestClassifyAgent_Working` augmented with Decision assertion at line 64: requires `item.Decision != nil && item.Decision.Outcome == broker.OutcomeUnknown`. No test logic rewritten, only assertions added.

**Merge conflict resolution:** No conflict markers detected. a63 source (758 lines) + fe23 additions (809 merged lines) show clean import addition (broker, progress packages) and struct/function extension. Original method bodies preserve flow, error handling, and classification logic.

**Broker contract verification:** 
- `agentDecision()` calls `broker.Unknown()` and constructs `broker.Decision` with proper Outcome and WaitReason
- Progress records initialized with Lane and Action correctly
- TriageWithEvidence preserves Decision through Result items
- No mutation of broker.Decision after construction; immutable return

### Tests run

```
cd /Users/kampe/Personal/Herdforge/.herd/review-surfaces/review-fac-790-fe23eeac33ae

# Focused attention integration tests (merge resolution scope)
go test -v ./pkg/attention -run 'TestAttention_RunWithFleet|TestTriageWithEvidence|TestClassifyProgress|TestClassifyAgent_Working|TestClassifyAgent_Starting' -timeout 5m -p 2

  TestAttention_RunWithFleet_OpencodeExportFinishLength: PASS
  TestAttention_RunWithFleet_IdentityFenceRejection: PASS
  TestAttention_RunWithFleet_MultipleHangingLanes_BoundedAggregateContext: PASS (15.00s)
  TestAttention_RunWithFleet_BlockingInitialCensus_ReturnsAtDeadline: PASS (15.00s)
  TestAttention_RunWithFleet_BlockingAfterCensus_ReturnsAtDeadline: PASS (15.00s)
  TestAttention_RunWithFleet_ModelRouteMismatch_RefusesEvidence: PASS
  TestAttention_RunWithFleet_ContextCensusCancellation_PropagatesToUnderlyingOperation: PASS (15.00s)
  TestAttention_RunWithFleet_UnboundRoute_RefusesEvidence: PASS
  TestAttention_RunWithFleet_UnmarshaledNativeHerdrTransport_AcceptsEvidence: PASS
  TestAttention_RunWithFleet_RealNativeHerdrJSONWithoutRouteFields_BindsAcceptedLaunchProvenance: PASS
  TestAttention_RunWithFleet_RealNativeHerdrJSON_MissingProvenance_RemainsUnknown: PASS
  TestTriageWithEvidence: PASS
  TestClassifyProgressEventWaitIsNotUsefulWork: PASS
  TestClassifyAgent_Working: PASS (includes Decision assertion)
  TestClassifyAgent_Starting: PASS

  Result: PASS ok github.com/Kampe/Herdforge/pkg/attention 60.178s
```

### Regression proof

Merger identity verified by inspection: agentDecision adds decision assignment AFTER original item construction, preserving all prior logic. TriageWithEvidence calls ClassifyAgentWithEvidence unmodified. No semantic loss in status classification, provider death handling, or held state logic.

Original Google CI (FAC-786/2239) confirmed via packet retained evidence: make ci exit 0, all gates pass.

### Residual acceptance gaps

Production identity of broker.Decision routes pending coordinator verification. agentDecision constructs decisions but integration into dispatch authority owned by FAC-581 (broker planning). Attention Decision field is informational observation, not autonomous dispatch.

---

patch_id: fab73977901b1c1a25057c48483d1afa33bc72a1
verification_digest: sha256:30256dea252f85b036b6c1dd6d4ab5ca141e0429f11892d8598e2fbdf33174ba
full_range: 61d9e868a8533a6c70eff09024d7ad25bc0f9a27..fe23eeac33ae87c467cf84fe67c0c277e86a72a6
