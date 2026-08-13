package residual

import (
	"context"
	"errors"
	"testing"
)

type fakeProvider struct {
	followups []FollowUp
	createN   int
	readErr   error
}

func (p *fakeProvider) FindResidualFollowUps(_ context.Context, _, id string) ([]FollowUp, error) {
	var out []FollowUp
	for _, f := range p.followups {
		if f.ResidualID == id {
			out = append(out, f)
		}
	}
	return out, nil
}
func (p *fakeProvider) CreateResidualFollowUp(_ context.Context, r Record) (FollowUp, error) {
	p.createN++
	f := FollowUp{ID: "follow-1", Ref: "FAC-238", ResidualID: r.ID, EvidenceRef: r.EvidenceRef}
	p.followups = append(p.followups, f)
	return f, nil
}
func (p *fakeProvider) ReadResidualFollowUp(_ context.Context, id string) (FollowUp, error) {
	if p.readErr != nil {
		return FollowUp{}, p.readErr
	}
	for _, f := range p.followups {
		if f.ID == id {
			return f, nil
		}
	}
	return FollowUp{}, errors.New("not found")
}

func testRecord(t *testing.T, required bool) Record {
	t.Helper()
	r, err := New(KindDeferredFunction, SeverityMedium, "bounded rollback", "task-237", "FAC-237", "revision-7", "receipt:abc", required)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEnsureFollowUpDeduplicatesAndRequiresReadback(t *testing.T) {
	p := &fakeProvider{}
	r, err := EnsureFollowUp(context.Background(), p, testRecord(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if p.createN != 1 || r.FollowUpRef != "FAC-238" || r.LinkEvidence == "" {
		t.Fatalf("missing linked readback: creates=%d record=%+v", p.createN, r)
	}
	// Re-entering after a crash finds the same provider record; it must not
	// produce another card. This fails against a create-first implementation.
	_, err = EnsureFollowUp(context.Background(), p, testRecord(t, false))
	if err != nil || p.createN != 1 {
		t.Fatalf("dedupe failed: err=%v creates=%d", err, p.createN)
	}

	p.readErr = errors.New("timeout")
	if _, err := EnsureFollowUp(context.Background(), p, testRecord(t, false)); !errors.Is(err, ErrMissingLinkage) {
		t.Fatalf("missing readback accepted: %v", err)
	}
}

func TestValidateExitRequiredCriterionNeverBecomesCompletion(t *testing.T) {
	p := &fakeProvider{}
	required, err := EnsureFollowUp(context.Background(), p, testRecord(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExit([]Record{required}, "revision-7"); !errors.Is(err, ErrRequired) {
		t.Fatalf("required residual waived acceptance: %v", err)
	}

	unlinked := testRecord(t, false)
	if err := ValidateExit([]Record{unlinked}, "revision-7"); !errors.Is(err, ErrMissingLinkage) {
		t.Fatalf("unlinked residual accepted: %v", err)
	}
}

func TestRecordIdentityBindsRevisionAndEvidence(t *testing.T) {
	r := testRecord(t, false)
	r.EvidenceRef = "receipt:replaced"
	if err := r.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mutated evidence retained authority: %v", err)
	}
}
