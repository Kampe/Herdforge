package launch

import (
	"strings"
	"testing"
	"time"
)

func TestAcceptedCanonicalMemberRequiresLogMembership(t *testing.T) {
	canonical := Receipt{
		TaskRef: "FAC-765", Role: ReviewerRole, Name: "review-fac-999",
		DecisionDigest: "review-digest-001", PaneID: "W4-canonical-review-pane",
		HerdrSession: "w4-review-session", ProcessIdentity: "W4-canonical-review-pane",
		Accepted: true, StartToken: "review-start",
	}
	got, err := AcceptedCanonicalMember([]Receipt{canonical}, canonical)
	if err != nil {
		t.Fatalf("exact member: %v", err)
	}
	if got.PaneID != canonical.PaneID || got.StartToken != canonical.StartToken {
		t.Fatalf("canonical copy PaneID=%q StartToken=%q", got.PaneID, got.StartToken)
	}

	forged := canonical
	forged.PaneID = "W4-forged-never-ran"
	forged.ProcessIdentity = "W4-forged-never-ran"
	forged.HerdrSession = "W4-forged-never-ran"
	forged.DecisionDigest = "forged-digest-never-ran"
	if _, err := AcceptedCanonicalMember([]Receipt{canonical}, forged); err == nil || !strings.Contains(err.Error(), "canonical accepted member") {
		t.Fatalf("forged locator err=%v", err)
	}
}

func TestAcceptedCanonicalMemberRefusesTamperedStartToken(t *testing.T) {
	canonical := Receipt{
		TaskRef: "FAC-765", Role: ReviewerRole, Name: "review-fac-999",
		DecisionDigest: "review-digest-001", PaneID: "W4-canonical-review-pane",
		HerdrSession: "w4-review-session", ProcessIdentity: "W4-canonical-review-pane",
		Accepted: true, StartToken: "review-start",
	}
	locator := canonical
	locator.StartToken = "tampered-start-token"
	_, err := AcceptedCanonicalMember([]Receipt{canonical}, locator)
	if err == nil {
		t.Fatal("copied DecisionDigest with a tampered StartToken must not authenticate")
	}
	if !strings.Contains(err.Error(), "start token") {
		t.Fatalf("tampered StartToken err=%v, want start-token refusal", err)
	}
	if strings.Contains(err.Error(), "mismatched host") || strings.Contains(err.Error(), "mismatched session") {
		t.Fatalf("StartToken replay must not be diagnosed as host/session mismatch: %v", err)
	}
}

func TestAcceptedCanonicalMemberRefusesMismatchedHostAndSession(t *testing.T) {
	canonical := Receipt{
		TaskRef: "FAC-765", Role: ReviewerRole, Name: "review-fac-999",
		DecisionDigest: "review-digest-001", PaneID: "W4-canonical-review-pane",
		HerdrSession: "w4-review-session", ProcessIdentity: "W4-canonical-review-pane",
		Accepted: true,
	}
	hostSpoof := canonical
	hostSpoof.PaneID = "W4-mismatched-host"
	if _, err := AcceptedCanonicalMember([]Receipt{canonical}, hostSpoof); err == nil || !strings.Contains(err.Error(), "mismatched host") {
		t.Fatalf("mismatched host err=%v", err)
	}
	sessionSpoof := canonical
	sessionSpoof.HerdrSession = "mismatched-review-session"
	if _, err := AcceptedCanonicalMember([]Receipt{canonical}, sessionSpoof); err == nil || !strings.Contains(err.Error(), "mismatched session") {
		t.Fatalf("mismatched session err=%v", err)
	}
}

func TestAcceptedReviewLaunchForRejectsBuilderIdentity(t *testing.T) {
	builder := Receipt{
		TaskRef: "FAC-765", Role: WorkerRole, Name: "review-fac-999",
		PaneID: "builder-pane-not-reviewer", ProcessIdentity: "builder-proc",
		Accepted: true, BuilderFamily: "openai",
	}
	if _, err := AcceptedReviewLaunchFor([]Receipt{builder}, "review-fac-999"); err == nil || !strings.Contains(err.Error(), "review launch") {
		t.Fatalf("builder posing as reviewer err=%v", err)
	}
	review := Receipt{
		TaskRef: "FAC-765", Role: ReviewerRole, Name: "review-fac-999",
		PaneID: "W4-canonical-review-pane", ProcessIdentity: "W4-canonical-review-pane",
		Accepted: true,
	}
	got, err := AcceptedReviewLaunchFor([]Receipt{builder, review}, "review-fac-999")
	if err != nil {
		t.Fatalf("review launch: %v", err)
	}
	if got.PaneID != "W4-canonical-review-pane" {
		t.Fatalf("review PaneID=%q", got.PaneID)
	}
}

func TestAcceptedReviewLaunchForRefusesConflictingHosts(t *testing.T) {
	a := Receipt{Role: ReviewerRole, Name: "review-fac-999", PaneID: "host-a", Accepted: true}
	b := Receipt{Role: ReviewerRole, Name: "review-fac-999", PaneID: "host-b", Accepted: true}
	if _, err := AcceptedReviewLaunchFor([]Receipt{a, b}, "review-fac-999"); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting review hosts err=%v", err)
	}
}

func TestReachingBuilderReceiptIgnoresLocatorFamily(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/launch-receipts.jsonl"
	commit := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	sink := &JSONLSink{Path: path}
	if err := sink.Write(Receipt{
		CreatedAt: time.Date(2026, 9, 7, 3, 9, 34, 0, time.UTC),
		Accepted:  true, Branch: "fix/fac-999", BuilderFamily: "openai",
		Role: WorkerRole, Name: "fac-765-builder",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := ReachingBuilderReceipt(path, "abc", commit, func(branch string) bool { return branch == "fix/fac-999" })
	if !ok || got.BuilderFamily != "openai" {
		t.Fatalf("reaching builder=%+v ok=%v", got, ok)
	}
	family, ok := BuilderFamilyReachingSHA(path, "abc", commit, func(branch string) bool { return branch == "fix/fac-999" })
	if !ok || family != "openai" {
		t.Fatalf("family=%q ok=%v", family, ok)
	}
}
