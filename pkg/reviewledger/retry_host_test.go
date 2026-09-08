package reviewledger

import (
	"strings"
	"testing"
)

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
