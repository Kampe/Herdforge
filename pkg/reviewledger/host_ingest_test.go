package reviewledger

import (
	"strings"
	"testing"
	"time"
)

const (
	fac652SHA      = "2a3a20d57ba7e17f923d0260ed60edfff3fe27f9"
	fac670SHA      = "02fd978e46581cb6d89e3fcb84502db1da52785a"
	fac652Rev      = "review-fac-652-2a3a20d57ba7"
	localDig       = "465571760dd09435214e1798905839079d1e02dc6ea741d49da0d95fbc77bc7c"
	w4XAIDig       = "e2624fb531c84a55cbcfbc346b2ff39b75c0c63d510744bd745daaa4af8fd5c1"
	w4AnthropicDig = "c36bc4611af5f570506e8e46808c635992d2b032660d1f8dd98c13a526efa70c"
	prodParent     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDeltaBase  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func seedLocalGooglePass(t *testing.T, l *Ledger, sha, reviewer string) LedgerRow {
	t.Helper()
	if err := l.Record(RecordOpts{
		SHA: sha, Branch: "fix/fac-652-direct", Reviewer: reviewer, Task: "FAC-652",
		BuilderFamily: "openai", ReviewerFamily: "google", Gate: "independent",
		Artifact: "local-google.md",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{
		SHA: sha, Reviewer: reviewer, Task: "FAC-652", Branch: "fix/fac-652-direct",
		Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai",
		ArtifactDigest: localDig, Artifact: "local-google.md", VfyDigest: "local-vfy",
		CandidateSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}
	prior, found, err := l.VerdictForReviewer(sha, reviewer)
	if err != nil || !found {
		t.Fatalf("seed prior: found=%v err=%v", found, err)
	}
	return prior
}

func fac652CommitTime() time.Time {
	return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
}

func reachingReceipt(string) LaunchProvenance {
	return LaunchProvenance{
		Host: "w4-session-pane", Session: "w4-session-pane",
		BuilderFamily: "openai", Branch: "fix/fac-652-direct",
		CreatedAt: time.Date(2026, 9, 7, 3, 9, 34, 0, time.UTC),
		Accepted:  true,
		Member:    true,
	}
}

func reachesFixFac652(branch, sha string) bool {
	return branch == "fix/fac-652-direct" && sha == fac652SHA
}

func wholeRangeXAI(t *testing.T, l *Ledger) (bool, error) {
	t.Helper()
	return l.HostIngest(HostIngestOpts{
		SHA: fac652SHA, Reviewer: fac652Rev, Task: "FAC-652", Branch: "fix/fac-652-direct",
		Artifact: "w4-xai.md", ArtifactDigest: w4XAIDig, Verdict: VerdictPASS,
		ReviewerFamily: "xai", BuilderFamily: FamilyUnrecorded, VfyDigest: "w4-xai-vfy",
		ReadBase: prodParent, ReadHead: fac652SHA, ProductionBase: prodParent,
		CommitTime: fac652CommitTime(), Reaches: reachesFixFac652,
		Receipt: reachingReceipt(fac652SHA),
	})
}

func TestHostIngestReconcilesW4XAIWholeRangeWithoutRewritingLocalGoogle(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	prior := seedLocalGooglePass(t, l, fac652SHA, fac652Rev)
	before, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}

	enqueued, err := wholeRangeXAI(t, l)
	if err != nil {
		t.Fatalf("host ingest W4 XAI: %v", err)
	}
	if !enqueued {
		t.Fatal("W4 XAI whole-range PASS must enqueue the branch-bound candidate")
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) <= len(before) {
		t.Fatalf("expected append, rows %d -> %d", len(before), len(rows))
	}
	if prior.ReviewerFamily != "google" {
		t.Fatalf("seed prior family = %q", prior.ReviewerFamily)
	}
	if !hasExactVerdict(rows, fac652SHA, fac652Rev, "", "google", localDig) {
		t.Fatalf("local google PASS was rewritten or dropped: %+v", rows)
	}
	if !hasExactVerdict(rows, fac652SHA, fac652Rev, "w4-session-pane", "xai", w4XAIDig) {
		t.Fatalf("W4 XAI PASS was not appended with reconciled identity: %+v", rows)
	}
	w4 := verdictByHost(rows, "w4-session-pane")
	if w4.BuilderFamily != "openai" {
		t.Fatalf("unrecorded builder family was not reconciled from reaching receipt: %+v", w4)
	}
	if w4.ReviewerFamily == "google" {
		t.Fatalf("false-bound W4 XAI onto local google: %+v", w4)
	}
}

func TestHostIngestRefusesAnthropicTestDeltaAsProductionParentReview(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedLocalGooglePass(t, l, fac652SHA, fac652Rev)
	before, _ := l.AllRows()

	_, err = l.HostIngest(HostIngestOpts{
		SHA: fac652SHA, Reviewer: fac652Rev, Task: "FAC-652", Branch: "fix/fac-652-direct",
		Artifact: "w4-anthropic-testdelta.md", ArtifactDigest: w4AnthropicDig, Verdict: VerdictPASS,
		ReviewerFamily: "anthropic", BuilderFamily: "openai", VfyDigest: "c36-vfy",
		ReadBase: testDeltaBase, ReadHead: fac652SHA, ProductionBase: prodParent,
		CommitTime: fac652CommitTime(), Reaches: reachesFixFac652,
		Receipt: reachingReceipt(fac652SHA),
	})
	if err == nil || !strings.Contains(err.Error(), "test-delta") {
		t.Fatalf("c36 test-delta error = %v, want production-parent refusal", err)
	}
	after, _ := l.AllRows()
	if len(after) != len(before) {
		t.Fatalf("test-delta ingest mutated history: %d -> %d", len(before), len(after))
	}
	if hasExactVerdict(after, fac652SHA, fac652Rev, "w4-session-pane", "anthropic", w4AnthropicDig) {
		t.Fatal("laundered c36 anthropic test-delta as whole-range review")
	}
}

func TestHostIngestDuplicateSameReviewerSameHostIsNoop(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedLocalGooglePass(t, l, fac652SHA, fac652Rev)
	first, err := wholeRangeXAI(t, l)
	if err != nil || !first {
		t.Fatalf("first ingest enqueued=%v err=%v", first, err)
	}
	before, _ := l.AllRows()
	again, err := wholeRangeXAI(t, l)
	if err != nil {
		t.Fatalf("duplicate ingest: %v", err)
	}
	if again {
		t.Fatal("identical host-labelled replay must not enqueue again")
	}
	after, _ := l.AllRows()
	if len(after) != len(before) {
		t.Fatalf("duplicate same-reviewer reassessment appended history: %d -> %d", len(before), len(after))
	}
}

func TestHostIngestAuthenticatedBuilderFamilyCorrectionPreservesVerdict(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	prior := seedLocalGooglePass(t, l, fac670SHA, "review-fac-670")
	before, _ := l.AllRows()
	reaches := func(branch, sha string) bool {
		return branch == "fix/fac-652-direct" && sha == fac670SHA
	}
	_, err = l.HostIngest(HostIngestOpts{
		SHA: fac670SHA, Reviewer: "review-fac-670", Task: "FAC-670",
		CommitTime: fac652CommitTime(), Reaches: reaches,
		Receipt: LaunchProvenance{
			Host: "w4-session-pane", Session: "w4-session-pane",
			BuilderFamily: "openai", Branch: "fix/fac-652-direct",
			CreatedAt: time.Date(2026, 9, 7, 3, 9, 34, 0, time.UTC), Accepted: true, Member: true,
		},
	})
	if err != nil {
		t.Fatalf("builder-family correction: %v", err)
	}
	after, _ := l.AllRows()
	if !hasExactVerdict(after, fac670SHA, "review-fac-670", "", prior.ReviewerFamily, prior.ArtifactDigest) {
		t.Fatalf("correction rewrote historical verdict: %+v", after)
	}
	foundRecord := false
	for _, r := range after {
		if r.Event == string(EventRecord) && r.SHA == fac670SHA && r.Host == "w4-session-pane" && r.BuilderFamily == "openai" {
			foundRecord = true
		}
	}
	if !foundRecord {
		t.Fatalf("authenticated builder-family record was not appended: %+v", after)
	}
	if len(after) <= len(before) {
		t.Fatal("correction produced no append-only provenance")
	}
	verdicts := 0
	for _, r := range after {
		if r.Event == string(EventVerdict) && r.SHA == fac670SHA {
			verdicts++
			if r.ReviewerFamily != "google" {
				t.Fatalf("correction relabelled verdict family: %+v", r)
			}
		}
	}
	if verdicts != 1 {
		t.Fatalf("correction must not add or rewrite verdicts, got %d", verdicts)
	}
}

func TestHostIngestRefusesNonmemberProvenance(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedLocalGooglePass(t, l, fac652SHA, fac652Rev)
	before, _ := l.AllRows()
	p := reachingReceipt(fac652SHA)
	p.Member = false
	_, err = l.HostIngest(HostIngestOpts{
		SHA: fac652SHA, Reviewer: fac652Rev, Task: "FAC-652",
		CommitTime: fac652CommitTime(), Reaches: reachesFixFac652,
		Receipt: p,
	})
	if err == nil || !strings.Contains(err.Error(), "canonical accepted member") {
		t.Fatalf("nonmember provenance error = %v", err)
	}
	after, _ := l.AllRows()
	if len(after) != len(before) {
		t.Fatalf("nonmember ingest mutated history: %d -> %d", len(before), len(after))
	}
}

func TestHostIngestRefusesMismatchUnreachableAndFlagOnlyTrust(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedLocalGooglePass(t, l, fac652SHA, fac652Rev)
	before, _ := l.AllRows()
	base := HostIngestOpts{
		SHA: fac652SHA, Reviewer: fac652Rev, Task: "FAC-652",
		Artifact: "x.md", ArtifactDigest: w4XAIDig, Verdict: VerdictPASS,
		ReviewerFamily: "xai", BuilderFamily: FamilyUnrecorded,
		ReadBase: prodParent, ReadHead: fac652SHA, ProductionBase: prodParent,
		CommitTime: fac652CommitTime(), Reaches: reachesFixFac652,
		Receipt: reachingReceipt(fac652SHA),
	}

	shaMismatch := base
	shaMismatch.SHA = fac670SHA
	shaMismatch.Receipt.CandidateSHA = fac652SHA
	if _, err := l.HostIngest(shaMismatch); err == nil || !strings.Contains(err.Error(), "SHA/reviewer mismatch") {
		t.Fatalf("SHA mismatch error = %v", err)
	}

	reviewerMismatch := base
	reviewerMismatch.Reviewer = "someone-else"
	reviewerMismatch.Receipt = reachingReceipt(fac652SHA)
	if _, err := l.HostIngest(reviewerMismatch); err == nil || !strings.Contains(err.Error(), "SHA/reviewer mismatch") {
		t.Fatalf("reviewer mismatch error = %v", err)
	}

	noHost := base
	noHost.Receipt.Host = ""
	noHost.Receipt.Session = ""
	if _, err := l.HostIngest(noHost); err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("missing host error = %v", err)
	}

	late := base
	late.Receipt.CreatedAt = fac652CommitTime().Add(time.Hour)
	if _, err := l.HostIngest(late); err == nil || !strings.Contains(err.Error(), "predate") {
		t.Fatalf("late receipt error = %v", err)
	}

	miss := base
	miss.Reaches = func(string, string) bool { return false }
	if _, err := l.HostIngest(miss); err == nil || !strings.Contains(err.Error(), "does not reach") {
		t.Fatalf("unreachable receipt error = %v", err)
	}

	contradict := base
	contradict.BuilderFamily = "anthropic"
	if _, err := l.HostIngest(contradict); err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("family contradiction error = %v", err)
	}

	after, _ := l.AllRows()
	if len(after) != len(before) {
		t.Fatalf("refusals mutated history: %d -> %d", len(before), len(after))
	}
	if !hasExactVerdict(after, fac652SHA, fac652Rev, "", "google", localDig) {
		t.Fatal("refusals rewrote the local google PASS")
	}
}

func hasExactVerdict(rows []LedgerRow, sha, reviewer, host, reviewerFamily, digest string) bool {
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == sha && r.Reviewer == reviewer &&
			strings.TrimSpace(r.Host) == host && r.ReviewerFamily == reviewerFamily && r.ArtifactDigest == digest {
			return true
		}
	}
	return false
}

func verdictByHost(rows []LedgerRow, host string) LedgerRow {
	var out LedgerRow
	for _, r := range rows {
		if r.Event == string(EventVerdict) && strings.TrimSpace(r.Host) == host {
			out = r
		}
	}
	return out
}
