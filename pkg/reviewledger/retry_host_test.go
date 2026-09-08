package reviewledger

import (
	"strings"
	"testing"
)

func mustRows(t *testing.T, l *Ledger) []LedgerRow {
	t.Helper()
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSameHostRetryReachesEveryFinalConsumer(t *testing.T) {
	l := newTestLedger(t)
	const sha = "same-host-retry-final"
	const host = "host-a"
	mustErr(l.Record(RecordOpts{
		SHA: sha, Reviewer: "reviewer-a", Host: host, Branch: "main",
		BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
		ReviewerFamily: admitReviewFm, Gate: "independent", Tier: "R2", Task: admitTask,
	}))
	mustErr(l.Record(RecordOpts{
		SHA: sha, Reviewer: "reviewer-b", Host: host, Branch: "main",
		BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
		ReviewerFamily: admitReviewFm, Gate: "independent", Tier: "R2", Task: admitTask,
	}))
	must2(l.Verdict(VerdictOpts{
		SHA: sha, Reviewer: "reviewer-a", Host: host, Verdict: VerdictFAIL,
		ReviewerFamily: admitReviewFm, BuilderFamily: admitAuthorFm,
		Task: admitTask, Lease: admitLease, PatchURL: admitPatch, CandidateSHA: sha,
	}))
	must2(l.Verdict(VerdictOpts{
		SHA: sha, Reviewer: "reviewer-b", Host: host, Verdict: VerdictPASS,
		ReviewerFamily: admitReviewFm, BuilderFamily: admitAuthorFm,
		Task: admitTask, Lease: admitLease, PatchURL: admitPatch, VfyDigest: admitDigest,
		ArtifactDigest: strings.Repeat("ab", 32), CandidateSHA: sha, RetryOf: "reviewer-a",
	}))
	eligible, err := l.Eligible(sha, admitAuthorFm)
	if err != nil || !eligible {
		t.Fatalf("Eligible: eligible=%v err=%v", eligible, err)
	}
	ready, rerr := l.MergeReadinessFor(sha)
	if rerr != nil || !ready.Ready {
		t.Fatalf("MergeReadiness: ready=%+v err=%v", ready, rerr)
	}
	reduced, rerr := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: sha})
	if rerr != nil && (reduced == nil || !reduced.Admitted) {
		t.Fatalf("AdmitReduced: %+v err=%v", reduced, rerr)
	}
	if reduced == nil || !reduced.Admitted {
		t.Fatalf("AdmitReduced not admitted: %+v", reduced)
	}
	full, ferr := l.Admit(AdmissionOpts{
		CandidateSHA: sha, Task: admitTask, Lease: admitLease, PatchURL: admitPatch,
		AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
	})
	if ferr != nil && (full == nil || !full.Admitted) {
		t.Fatalf("Admit: %+v err=%v", full, ferr)
	}
	if full == nil || !full.Admitted {
		t.Fatalf("Admit not admitted: %+v", full)
	}
	cerr := l.CompleteAdmissionRecord(admitTask, sha, "reviewer-b", func(LedgerRow) (RecordCompletion, error) {
		return RecordCompletion{Branch: "main", Tier: "R2"}, nil
	})
	if cerr != nil {
		t.Fatalf("CompleteAdmissionRecord: %v", cerr)
	}
	vetoes, verr := l.VetoSHAs()
	if verr != nil {
		t.Fatal(verr)
	}
	for _, v := range vetoes {
		if v == sha {
			t.Fatalf("VetoSHAs still lists superseded FAIL: %v", vetoes)
		}
	}
	passes, contradict, berr := independentPassTargets(l, mustRows(t, l), sha, admitTask)
	if berr != nil || contradict || len(passes) != 1 || passes[0].Reviewer != "reviewer-b" {
		t.Fatalf("BindEvidence targets: passes=%+v contradict=%v err=%v", passes, contradict, berr)
	}
}

func TestCrossHostRetryOrderPreservesVetoInBothAppendOrders(t *testing.T) {
	orders := [][]string{{"fail-first", "pass-second"}, {"pass-first", "fail-second"}}
	for _, order := range orders {
		t.Run(strings.Join(order, "_"), func(t *testing.T) {
			l := newTestLedger(t)
			const sha = "cross-host-order"
			mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
			mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", BuilderFamily: "anthropic", ReviewerFamily: "xai", Gate: "independent"}))
			fail := VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}
			pass := VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", Verdict: VerdictPASS, ReviewerFamily: "xai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}
			if order[0] == "fail-first" {
				must2(l.Verdict(fail))
				must2(l.Verdict(pass))
			} else {
				must2(l.Verdict(pass))
				must2(l.Verdict(fail))
			}
			eligible, err := l.Eligible(sha, "anthropic")
			if err == nil || eligible {
				t.Fatalf("order %v eligible=%v err=%v", order, eligible, err)
			}
			ready, _ := l.MergeReadinessFor(sha)
			if ready.Ready {
				t.Fatalf("order %v readiness ready", order)
			}
			reduced, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: sha})
			if reduced != nil && reduced.Admitted {
				t.Fatalf("order %v AdmitReduced admitted", order)
			}
		})
	}
}

func TestEligibleCrossHostRetryDoesNotHideHostAVeto(t *testing.T) {
	l := newTestLedger(t)
	const sha = "cross-host-retry-sha"
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", BuilderFamily: "anthropic", ReviewerFamily: "xai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", Verdict: VerdictPASS, ReviewerFamily: "xai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}); err != nil {
		t.Fatal(err)
	}
	eligible, err := l.Eligible(sha, "anthropic")
	if err == nil || eligible {
		t.Fatalf("host-B retry must not hide host-A FAIL: eligible=%v err=%v", eligible, err)
	}
	if !strings.Contains(err.Error(), "veto") {
		t.Fatalf("want veto refusal, got %v", err)
	}
	queued, qerr := l.Queued()
	if qerr != nil {
		t.Fatal(qerr)
	}
	for _, q := range queued {
		if q.SHA == sha {
			t.Fatalf("queued hid host-A FAIL behind host-B RetryOf: %+v", queued)
		}
	}
	ready, rerr := l.MergeReadinessFor(sha)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if ready.Ready {
		t.Fatalf("readiness hid host-A FAIL: %+v", ready)
	}
}

func TestEligibleSameHostRetryClearsNamedVeto(t *testing.T) {
	l := newTestLedger(t)
	const sha = "same-host-retry-sha"
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-b", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-b", Host: "host-a", Verdict: VerdictPASS, ReviewerFamily: "openai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}); err != nil {
		t.Fatal(err)
	}
	eligible, err := l.Eligible(sha, "anthropic")
	if err != nil || !eligible {
		t.Fatalf("same-host authorized retry must clear host-A FAIL: eligible=%v err=%v", eligible, err)
	}
}

func TestEligibleCrossHostBlockedSurvivesRetry(t *testing.T) {
	l := newTestLedger(t)
	const sha = "cross-host-blocked-sha"
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", BuilderFamily: "anthropic", ReviewerFamily: "xai", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictBLOCKED, ReviewerFamily: "openai", BuilderFamily: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", Verdict: VerdictPASS, ReviewerFamily: "xai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}); err != nil {
		t.Fatal(err)
	}
	eligible, err := l.Eligible(sha, "anthropic")
	if err == nil || eligible {
		t.Fatalf("host-B retry must not hide host-A BLOCKED: eligible=%v err=%v", eligible, err)
	}
}

func TestGenericSupersessionDoesNotClearCurrentVeto(t *testing.T) {
	l := newTestLedger(t)
	const current = "generic-supersession-current"
	const previous = "generic-supersession-previous"
	mustErr(l.Record(RecordOpts{SHA: current, Reviewer: "reviewer-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
	mustErr(l.Record(RecordOpts{SHA: current, Reviewer: "reviewer-b", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
	must2(l.Verdict(VerdictOpts{SHA: current, Reviewer: "reviewer-a", Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}))
	must2(l.Verdict(VerdictOpts{SHA: current, Reviewer: "reviewer-b", Verdict: VerdictPASS, ReviewerFamily: "openai", BuilderFamily: "anthropic"}))
	mustErr(l.Supersession(DecisionOpts{
		SHA: current, PreviousSHA: previous, Reviewer: "reviewer-a",
		Reason: "candidate identity replacement",
	}))
	eligible, err := l.Eligible(current, "anthropic")
	if err == nil || eligible {
		t.Fatalf("generic EventSupersession cleared current veto: eligible=%v err=%v", eligible, err)
	}

	t.Run("bound_same_host_retry_still_eligible", func(t *testing.T) {
		l := newTestLedger(t)
		const sha = "bound-retry-still-eligible"
		const host = "host-a"
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: host, BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-b", Host: host, BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: host, Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-b", Host: host, Verdict: VerdictPASS, ReviewerFamily: "openai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}))
		ok, err := l.Eligible(sha, "anthropic")
		if err != nil || !ok {
			t.Fatalf("bound same-host PASS RetryOf: eligible=%v err=%v", ok, err)
		}
	})
	t.Run("other_host_fail_survives", func(t *testing.T) {
		l := newTestLedger(t)
		const sha = "generic-other-host-fail"
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", BuilderFamily: "anthropic", ReviewerFamily: "xai", Gate: "independent"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictFAIL, ReviewerFamily: "openai", BuilderFamily: "anthropic"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", Verdict: VerdictPASS, ReviewerFamily: "xai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}))
		ok, err := l.Eligible(sha, "anthropic")
		if err == nil || ok {
			t.Fatalf("other-host FAIL cleared: eligible=%v err=%v", ok, err)
		}
	})
	t.Run("other_host_blocked_survives", func(t *testing.T) {
		l := newTestLedger(t)
		const sha = "generic-other-host-blocked"
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"}))
		mustErr(l.Record(RecordOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", BuilderFamily: "anthropic", ReviewerFamily: "xai", Gate: "independent"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-a", Verdict: VerdictBLOCKED, ReviewerFamily: "openai", BuilderFamily: "anthropic"}))
		must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Host: "host-b", Verdict: VerdictPASS, ReviewerFamily: "xai", BuilderFamily: "anthropic", RetryOf: "reviewer-a"}))
		ok, err := l.Eligible(sha, "anthropic")
		if err == nil || ok {
			t.Fatalf("other-host BLOCKED cleared: eligible=%v err=%v", ok, err)
		}
	})
}
