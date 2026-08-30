package reviewledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-630: every qualification failure in AdmitReduced landed on one generic
// string, so an operator could not tell which of eight conditions failed.
//
// Live cost, twice in one night. CHA-3211 held a valid PASS and a real
// verification digest and was refused with "no independent PASS verdict with
// durable verification evidence" -- the digest was present; the launch row's
// builder_family was "unrecorded". CHA-3465 then cleared that gate and got the
// identical string, this time because no risk tier was on record. Both read as
// "the digest is missing". Absence of a reason is not a reason.

func diagLedger(t *testing.T, rows ...map[string]any) *Ledger {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	var b strings.Builder
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewReadOnlyReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

const diagSHA = "0591cbac4ea6ea3f78c120a8b7831c87e5885b80"

func record(fam string, extra map[string]any) map[string]any {
	r := map[string]any{
		"event": "record", "sha": diagSHA, "reviewer": "rv",
		"builder_family": fam, "reviewer_family": "xai",
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func verdictRow(extra map[string]any) map[string]any {
	r := map[string]any{
		"event": "verdict", "sha": diagSHA, "reviewer": "rv", "verdict": "PASS",
		"builder_family": "anthropic", "reviewer_family": "xai",
		"verification_digest": "ed36f2b334068b23d4369721de85c3e1",
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// CHA-3211's shape: provenance recorded as "unrecorded", which is deliberately
// outside the allowlist. The refusal must say so instead of implicating the
// digest.
func TestRefusalNamesAnUnrecordedBuilderFamily(t *testing.T) {
	l := diagLedger(t, record("unrecorded", map[string]any{"tier": "R3"}), verdictRow(nil))

	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil || res.Admitted {
		if res == nil {
			t.Fatal("no result")
		}
	}
	if !strings.Contains(res.Reason, "unrecorded") {
		t.Fatalf("refusal does not name the unrecorded builder family, so an operator "+
			"reads it as a missing digest and investigates the wrong artifact.\nGot: %s", res.Reason)
	}
}

// CHA-3465's shape: everything correct except the risk tier, which the ingest
// path never writes. The refusal must name the tier.
func TestRefusalNamesAMissingRiskTier(t *testing.T) {
	l := diagLedger(t, record("anthropic", nil), verdictRow(nil))

	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil {
		t.Fatal("no result")
	}
	if res.Admitted {
		t.Fatal("admitted with no risk tier on record")
	}
	if !strings.Contains(res.Reason, "risk tier") {
		t.Fatalf("refusal does not name the missing risk tier.\nGot: %s", res.Reason)
	}
}

// A genuinely absent digest must still be named as such -- the diagnostic must
// discriminate, not just append every possible cause.
func TestRefusalNamesAMissingVerificationDigest(t *testing.T) {
	l := diagLedger(t,
		record("anthropic", map[string]any{"tier": "R3"}),
		verdictRow(map[string]any{"verification_digest": ""}))

	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil {
		t.Fatal("no result")
	}
	if !strings.Contains(res.Reason, "verification digest") {
		t.Fatalf("refusal does not name the missing verification digest.\nGot: %s", res.Reason)
	}
	if strings.Contains(res.Reason, "risk tier") {
		t.Fatalf("refusal blamed the risk tier, which IS present; the diagnostic must "+
			"discriminate rather than list every condition.\nGot: %s", res.Reason)
	}
}

// A same-family reviewer is not independent, and the refusal must say that
// rather than implicating evidence that is present.
func TestRefusalNamesANonIndependentReviewerFamily(t *testing.T) {
	l := diagLedger(t,
		record("anthropic", map[string]any{"tier": "R3", "reviewer_family": "anthropic"}),
		verdictRow(map[string]any{"reviewer_family": "anthropic"}))

	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil {
		t.Fatal("no result")
	}
	// The fragment must be one the OLD generic refusal cannot produce. That
	// refusal was "no independent PASS verdict with durable verification
	// evidence" — it already contains "independent", so accepting that substring
	// made this test pass against the pre-FAC-630 code it was written to pin.
	// A W4 reviewer proved it by restoring the pre-FAC-630 blob: four sibling
	// tests failed as intended and this one passed.
	if !strings.Contains(res.Reason, "equals the builder family") {
		t.Fatalf("refusal does not name the same-family reviewer as the cause. "+
			"A generic message mentioning \"independent\" is not the discriminating "+
			"diagnostic FAC-630 exists to provide.\nGot: %s", res.Reason)
	}
}

// With no verdict rows at all the refusal must say that, not list conditions
// about a verdict that does not exist.
func TestRefusalDistinguishesNoVerdictAtAll(t *testing.T) {
	l := diagLedger(t, record("anthropic", map[string]any{"tier": "R3"}))

	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil {
		t.Fatal("no result")
	}
	if !strings.Contains(res.Reason, "no verdict rows exist") {
		t.Fatalf("refusal does not distinguish an absent verdict from a disqualified one.\nGot: %s", res.Reason)
	}
}

// Every reduced-admission refusal must identify the failed condition and only
// the safe observed values needed to act on it. This drives AdmitReduced itself
// so a formatter that is never called cannot satisfy the regression.
func TestReducedAdmissionNamesEveryRefusalConditionAndObservedValue(t *testing.T) {
	tests := []struct {
		name string
		rows []map[string]any
		want []string
	}{
		{
			name: "no launch rows",
			rows: []map[string]any{verdictRow(nil)},
			want: []string{"admission_condition=launch_record", "launch_rows=0"},
		},
		{
			name: "no matching launch row",
			rows: []map[string]any{
				record("anthropic", map[string]any{"reviewer": "other", "tier": "R3"}),
				verdictRow(nil),
			},
			want: []string{"condition=launch_record", "launch_record_present=false", `reviewer="rv"`},
		},
		{
			name: "empty builder family",
			rows: []map[string]any{record("", map[string]any{"tier": "R3"}), verdictRow(nil)},
			want: []string{"condition=builder_family", `builder_family=""`},
		},
		{
			name: "unallowlisted builder family",
			rows: []map[string]any{record(FamilyUnrecorded, map[string]any{"tier": "R3"}), verdictRow(nil)},
			want: []string{"condition=builder_family", `builder_family="unrecorded"`, "allowlisted=false"},
		},
		{
			name: "reviewer family conflict",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3", "reviewer_family": "xai"}),
				verdictRow(map[string]any{"reviewer_family": "openai"}),
			},
			want: []string{"condition=reviewer_family", "reviewer_family_state=conflict", `launch_reviewer_family="xai"`, `verdict_reviewer_family="openai"`},
		},
		{
			name: "builder family conflict",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3"}),
				verdictRow(map[string]any{"builder_family": "openai"}),
			},
			want: []string{"condition=reviewer_family", "reviewer_family_state=conflict", `launch_builder_family="anthropic"`, `verdict_builder_family="openai"`},
		},
		{
			name: "reviewer family unset",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3", "reviewer_family": ""}),
				verdictRow(map[string]any{"reviewer_family": ""}),
			},
			want: []string{"condition=reviewer_family", "reviewer_family_state=unset", `reviewer_family=""`},
		},
		{
			name: "same family",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3", "reviewer_family": "anthropic"}),
				verdictRow(map[string]any{"reviewer_family": "anthropic"}),
			},
			want: []string{"condition=family_independence", `reviewer_family="anthropic"`, `builder_family="anthropic"`, "independent=false"},
		},
		{
			name: "reviewer family not allowlisted",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3", "reviewer_family": "unknown"}),
				verdictRow(map[string]any{"reviewer_family": "unknown"}),
			},
			want: []string{"condition=reviewer_family", `reviewer_family="unknown"`, "allowlisted=false"},
		},
		{
			name: "empty reviewer identity",
			rows: []map[string]any{
				record("anthropic", map[string]any{"reviewer": "", "tier": "R3"}),
				verdictRow(map[string]any{"reviewer": ""}),
			},
			want: []string{"condition=reviewer_identity", `reviewer=""`},
		},
		{
			name: "reviewer equals builder identity",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3", "builder_identity": "rv"}),
				verdictRow(nil),
			},
			want: []string{"condition=identity_independence", `reviewer="rv"`, "builder_identity_match=true", "independent=false"},
		},
		{
			name: "empty verification digest",
			rows: []map[string]any{
				record("anthropic", map[string]any{"tier": "R3"}),
				verdictRow(map[string]any{"verification_digest": ""}),
			},
			want: []string{"condition=verification_digest", `verification_digest=""`},
		},
		{
			name: "empty risk tier",
			rows: []map[string]any{record("anthropic", nil), verdictRow(nil)},
			want: []string{"condition=risk_tier", `risk_tier=""`},
		},
		{
			name: "coordinator reviewer",
			rows: []map[string]any{
				record("anthropic", map[string]any{"reviewer": "coordinator", "tier": "R3"}),
				verdictRow(map[string]any{"reviewer": "coordinator"}),
			},
			want: []string{"condition=reviewer_authority", `reviewer="coordinator"`, "coordinator=true", "independent=false"},
		},
		{
			name: "non pass verdict",
			rows: []map[string]any{
				record("anthropic", map[string]any{"reviewer": "coordinator", "tier": "R3"}),
				verdictRow(map[string]any{"reviewer": "coordinator", "verdict": "FAIL"}),
			},
			want: []string{"condition=verdict", `verdict="FAIL"`, `required="PASS"`},
		},
		{
			name: "no verdict rows",
			rows: []map[string]any{record("anthropic", map[string]any{"tier": "R3"})},
			want: []string{"admission_condition=verdict", "verdict_rows=0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := diagLedger(t, tc.rows...)
			res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
			if err == nil || res == nil || res.Admitted {
				t.Fatalf("expected refusal, got result=%+v err=%v", res, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.Reason, want) {
					t.Errorf("reason %q does not contain %q", res.Reason, want)
				}
			}
		})
	}
}

func TestReducedAdmissionNamesEarlyRefusalConditions(t *testing.T) {
	t.Run("candidate sha", func(t *testing.T) {
		l := diagLedger(t, record("anthropic", map[string]any{"tier": "R3"}))
		res, err := l.AdmitReduced(ReducedAdmissionOpts{})
		if err == nil || res == nil || !strings.Contains(res.Reason, `admission_condition=candidate_sha observed candidate_sha=""`) {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	})

	t.Run("consumed", func(t *testing.T) {
		dir := t.TempDir()
		l, err := NewReviewLedger(dir, DefaultPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Record(RecordOpts{SHA: diagSHA, Reviewer: "rv", BuilderFamily: "anthropic", ReviewerFamily: "xai", Tier: "R3"}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Verdict(VerdictOpts{SHA: diagSHA, Reviewer: "rv", Verdict: VerdictPASS, BuilderFamily: "anthropic", ReviewerFamily: "xai", VfyDigest: "digest"}); err != nil {
			t.Fatal(err)
		}
		if err := l.Consumed(diagSHA, "merged-sha"); err != nil {
			t.Fatal(err)
		}
		res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
		if err == nil || res == nil || !strings.Contains(res.Reason, "admission_condition=consumption observed consumed=true") {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	})

	t.Run("veto", func(t *testing.T) {
		l := diagLedger(t,
			record("anthropic", map[string]any{"tier": "R3"}),
			verdictRow(map[string]any{"verdict": "BLOCKED"}),
		)
		res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
		if err == nil || res == nil {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		for _, want := range []string{"admission_condition=veto", `verdict="BLOCKED"`, "superseded=false"} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("reason %q does not contain %q", res.Reason, want)
			}
		}
	})
}

func TestReducedAdmissionReadFailuresNameTheEvidenceSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakAt func(t *testing.T, l *Ledger)
		want    string
	}{
		{
			name: "ledger",
			breakAt: func(t *testing.T, l *Ledger) {
				t.Helper()
				if err := os.Remove(l.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(l.Path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "admission_condition=review_ledger observed ledger_readable=false",
		},
		{
			name: "queue",
			breakAt: func(t *testing.T, l *Ledger) {
				t.Helper()
				if err := os.Mkdir(l.QueuePath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "admission_condition=admission_queue observed queue_readable=false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := diagLedger(t, record("anthropic", map[string]any{"tier": "R3"}), verdictRow(nil))
			tc.breakAt(t, l)
			res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
			if err == nil || res != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("result=%+v err=%v, want %q", res, err, tc.want)
			}
		})
	}
}

func TestReducedAdmissionDiagnosticsDoNotExposeSecretBearingFields(t *testing.T) {
	const diagnosticFieldSentinel = "fac630-secret-sentinel"
	l := diagLedger(t,
		record("anthropic", map[string]any{
			"artifact": diagnosticFieldSentinel, "lease": diagnosticFieldSentinel, "patch_url": diagnosticFieldSentinel,
			"pid": diagnosticFieldSentinel, "tier": "R3",
		}),
		verdictRow(map[string]any{"verification_digest": ""}),
	)
	res, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if res == nil {
		t.Fatal("no result")
	}
	if strings.Contains(res.Reason, diagnosticFieldSentinel) {
		t.Fatalf("refusal leaked a secret-bearing field: %q", res.Reason)
	}
}

func TestReducedAdmissionVetoAlwaysOutranksAnIndependentPass(t *testing.T) {
	rows := []map[string]any{
		record("anthropic", map[string]any{"reviewer": "pass-reviewer", "tier": "R3"}),
		verdictRow(map[string]any{"reviewer": "pass-reviewer"}),
		record("anthropic", map[string]any{"reviewer": "veto-reviewer", "tier": "R3"}),
		verdictRow(map[string]any{"reviewer": "veto-reviewer", "verdict": "BLOCKED"}),
	}
	for i := 0; i < 100; i++ {
		l := diagLedger(t, rows...)
		res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
		if err == nil || res == nil || res.Admitted {
			t.Fatalf("iteration %d: veto did not refuse: result=%+v err=%v", i, res, err)
		}
		for _, want := range []string{"admission_condition=veto", `verdict="BLOCKED"`, `reviewer="veto-reviewer"`} {
			if !strings.Contains(res.Reason, want) {
				t.Fatalf("iteration %d: reason %q does not contain %q", i, res.Reason, want)
			}
		}
	}
}
