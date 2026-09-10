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

### Integration assessment: Merge conflict resolution (61d9e + a63 → fe23)

**Scope:** Google-authored conflict resolution in merge commit fe23eeac33ae. Verified via `git show --remerge-diff`: two-file resolution affecting `pkg/attention/attention.go` and `pkg/attention/attention_test.go` only. Independent review of a63 Anthropic source (terminal evidence features) by Google confirmed in prior FAC-2213.

**Actual conflict markers (remerge-diff evidence):**

1. **Import conflict (lines 34-40)**: Base 61d9e added `pkg/progress`; a63 Anthropic added `pkg/patterns` and `pkg/process`. Merger resolution keeps both: patterns/process from a63, progress from base. No loss.

2. **agentDecision function (lines 220-239)**: Sourced from BASE (61d9e, Google authorship). NOT from a63. Merger preserved this function when resolving Triage/TriageWithEvidence conflict. Function constructs `broker.Decision` with progress.Record from live agent status classification. No semantic change.

3. **Triage vs TriageWithEvidence function (lines 242+)**: Base had legacy `Triage()` func; a63 replaced with `TriageWithEvidence()` (Anthropic addition). Merger resolved by deleting base Triage and accepting a63 TriageWithEvidence signature. TriageWithEvidence calls ClassifyAgentWithEvidence at line 292, preserving all terminal evidence evaluation.

4. **Item struct Decision field**: Base (61d9e) defined; a63 had removed it; merged state keeps Decision field and integrates agentDecision assignment at line 179 in ClassifyAgentWithEvidence.

**Preserved contracts:**

- ClassifyAgentWithEvidence signature unchanged: receives (a, held, heldReason, providerDeath, ev, ctx, paneText, evErr)
- Returns Item with Decision field populated by agentDecision(name, status)
- TriageWithEvidence calls ClassifyAgentWithEvidence unmodified, returns Result containing Items with Decision
- broker.Decision immutable after construction; no mutations post-merge
- Terminal evidence evaluation (ev parameter) flows through unmodified to process.EvaluateEvidence

**Family attribution (corrected):**

- **Base 61d9e (Google)**: agentDecision, broker.Decision field, progress.Record wiring
- **a63 (Anthropic, independently Google-reviewed PASS2213)**: TriageWithEvidence, terminal evidence extraction, process patterns, output-limit classification
- **Merger fe23 (Google)**: Conflict resolution only — preserved agentDecision from base, accepted TriageWithEvidence from a63, maintained both import lines and Item struct

No new logic introduced by merger beyond conflict resolution. Both Google base and Anthropic a63 features preserved intact.

## Tests run

```
cd /Users/kampe/Personal/Herdforge/.herd/review-surfaces/review-fac-790-fe23eeac33ae

# Focused attention integration tests (merge resolution scope)
go test -v ./pkg/attention -run 'TestAttention_RunWithFleet|TestTriageWithEvidence|TestClassifyProgress|TestClassifyAgent_Working|TestClassifyAgent_Starting' -timeout 5m -p 2

TestAttention_RunWithFleet_OpencodeExportFinishLength: PASS (0.00s)
TestAttention_RunWithFleet_IdentityFenceRejection: PASS (0.00s)
TestAttention_RunWithFleet_MultipleHangingLanes_BoundedAggregateContext: PASS (15.00s)
TestAttention_RunWithFleet_BlockingInitialCensus_ReturnsAtDeadline: PASS (15.00s)
TestAttention_RunWithFleet_BlockingAfterCensus_ReturnsAtDeadline: PASS (15.00s)
TestAttention_RunWithFleet_ModelRouteMismatch_RefusesEvidence: PASS (0.00s)
TestAttention_RunWithFleet_ContextCensusCancellation_PropagatesToUnderlyingOperation: PASS (15.00s)
TestAttention_RunWithFleet_UnboundRoute_RefusesEvidence: PASS (0.00s)
TestAttention_RunWithFleet_UnmarshaledNativeHerdrTransport_AcceptsEvidence: PASS (0.00s)
TestAttention_RunWithFleet_RealNativeHerdrJSONWithoutRouteFields_BindsAcceptedLaunchProvenance: PASS (0.01s)
TestAttention_RunWithFleet_RealNativeHerdrJSON_MissingProvenance_RemainsUnknown: PASS (0.00s)
TestTriageWithEvidence: PASS (0.00s)
TestClassifyProgressEventWaitIsNotUsefulWork: PASS (0.00s)
TestClassifyAgent_Working: PASS (0.00s) — includes Decision assertion
TestClassifyAgent_Starting: PASS (0.00s)

ok github.com/Kampe/Herdforge/pkg/attention 60.178s
```

### Regression proof

Original Google CI (source FAC-786/2239) confirmed: make ci exit 0, all gates pass. Merger logic verified by remerge-diff: conflict resolution preserves both a63 and base logic without loss. No new mutations introduced by merger.

### Residual acceptance gaps

Production identity of broker.Decision routes and TriageWithEvidence dispatcher integration owned by FAC-581 (broker planning). Merger resolves structural conflicts; semantic acceptance pending coordinator verification.

---

patch_id: fab73977901b1c1a25057c48483d1afa33bc72a1
verification_digest: sha256:30256dea252f85b036b6c1dd6d4ab5ca141e0429f11892d8598e2fbdf33174ba
full_range: 61d9e868a8533a6c70eff09024d7ad25bc0f9a27..fe23eeac33ae87c467cf84fe67c0c277e86a72a6
