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
