package reviewledger

import (
	"fmt"
	"strings"
	"testing"
)

const admitRepeat = 100

func seedTieredGooglePass(t *testing.T, l *Ledger) {
	t.Helper()
	if err := l.Record(RecordOpts{
		SHA: fac652SHA, Branch: "fix/fac-652-direct", Reviewer: fac652Rev, Task: "FAC-652",
		BuilderFamily: "openai", ReviewerFamily: "google", Gate: "independent",
		Artifact: "local-google.md", Tier: "R3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{
		SHA: fac652SHA, Reviewer: fac652Rev, Task: "FAC-652", Branch: "fix/fac-652-direct",
		Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai",
		ArtifactDigest: localDig, Artifact: "local-google.md", VfyDigest: "local-vfy",
		CandidateSHA: fac652SHA,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAdmitReducedRefused(t *testing.T, l *Ledger) {
	t.Helper()
	var admitted int
	var sample string
	for i := 0; i < admitRepeat; i++ {
		res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: fac652SHA})
		if res != nil && res.Admitted {
			admitted++
			if sample == "" {
				sample = fmt.Sprintf("iteration %d reason=%q err=%v", i, res.Reason, err)
			}
		}
	}
	if admitted > 0 {
		t.Fatalf("AdmitReduced fail-opened on %d/%d iterations (map-order PASS before veto): %s", admitted, admitRepeat, sample)
	}
}

func TestAdmitReducedRefusesAuthenticatedCrossHostDissent(t *testing.T) {
	failDig := "fa" + w4XAIDig[2:]
	blockDig := "bb" + w4XAIDig[2:]
	cases := []struct {
		name string
		seq  []struct {
			host, family, digest string
			verdict              Verdict
		}
	}{
		{name: "FAIL_then_PASS", seq: []struct {
			host, family, digest string
			verdict              Verdict
		}{
			{"host-a", "anthropic", failDig, VerdictFAIL},
			{"host-b", "xai", w4XAIDig, VerdictPASS},
		}},
		{name: "PASS_then_FAIL", seq: []struct {
			host, family, digest string
			verdict              Verdict
		}{
			{"host-b", "xai", w4XAIDig, VerdictPASS},
			{"host-a", "anthropic", failDig, VerdictFAIL},
		}},
		{name: "BLOCKED_then_PASS", seq: []struct {
			host, family, digest string
			verdict              Verdict
		}{
			{"host-a", "anthropic", blockDig, VerdictBLOCKED},
			{"host-b", "xai", w4XAIDig, VerdictPASS},
		}},
		{name: "PASS_then_BLOCKED", seq: []struct {
			host, family, digest string
			verdict              Verdict
		}{
			{"host-b", "xai", w4XAIDig, VerdictPASS},
			{"host-a", "anthropic", blockDig, VerdictBLOCKED},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := NewReviewLedger(dir, DefaultPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			seedTieredGooglePass(t, l)
			for _, step := range tc.seq {
				hostIngestArtifact(t, l, step.host, step.digest, step.verdict, step.family)
			}
			if eligible, err := l.Eligible(fac652SHA, "openai"); err == nil || eligible {
				t.Fatalf("Eligible must refuse dissent: eligible=%v err=%v", eligible, err)
			}
			assertAdmitReducedRefused(t, l)
		})
	}
}

func TestAdmitReducedAdmitsAuthenticatedNoDissentPass(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedTieredGooglePass(t, l)
	if _, err := wholeRangeXAI(t, l); err != nil {
		t.Fatal(err)
	}
	var refused int
	for i := 0; i < admitRepeat; i++ {
		res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: fac652SHA})
		if res == nil || !res.Admitted {
			refused++
			t.Errorf("iteration %d: no-dissent PASS refused: res=%+v err=%v", i, res, err)
		}
	}
	if refused > 0 {
		t.Fatalf("no-dissent PASS refused on %d/%d iterations", refused, admitRepeat)
	}
}

func TestAdmitReducedSameHostReassessmentIsLatest(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedTieredGooglePass(t, l)
	if _, err := wholeRangeXAI(t, l); err != nil {
		t.Fatal(err)
	}
	res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: fac652SHA})
	if err != nil || res == nil || !res.Admitted {
		t.Fatalf("same-host-independent PASS must admit: res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Reason, "reduced") {
		t.Fatalf("reason=%q", res.Reason)
	}
}
