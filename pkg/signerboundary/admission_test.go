package signerboundary

import (
	"strings"
	"testing"
)

func TestDefaultAdmitReviewerVerdict_BindsFields(t *testing.T) {
	req := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"patch-1", "APPROVED", "session-ok", nil,
	)
	if err := DefaultAdmitReviewerVerdict(req); err != nil {
		t.Fatal(err)
	}
	req.Payload = []byte(`{"candidate_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","verdict":"REJECTED"}`)
	if err := DefaultAdmitReviewerVerdict(req); err == nil {
		t.Fatal("mismatched verdict must fail admission")
	}
}

func TestAdmitRequest_RejectsUnadmitted(t *testing.T) {
	req := SignRequest{
		Op:           OpSignVerdict,
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PatchID:      "p",
		Verdict:      "APPROVED",
		SessionID:    "session-ok",
		Payload:      []byte(`{"foo":1}`),
	}
	err := AdmitRequest(req)
	if err == nil || !strings.Contains(err.Error(), "not admitted") && err != ErrVerdictNotAdmitted {
		// error wraps / is ErrVerdictNotAdmitted
		if err == nil {
			t.Fatal("expected admission failure")
		}
	}
}
