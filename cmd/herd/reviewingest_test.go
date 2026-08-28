package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-647: the sweep must judge ingestion by the ledger's own admission
// identity (sha+reviewer), not by artifact FILENAME. Matching basenames made a
// verdict admitted under a different filename -- a retry, a re-push from another
// host, a rename in transport -- count as un-ingested forever. Measured live: of
// 599 inbox files, 596 had a verdict row, only 300 matched by basename, and 296
// were reported as a backlog that did not exist. The sweep printed
// "admitted=296" while every line said DUPLICATE and the count never moved.
func TestSweepTreatsAVerdictedSHAAsIngestedRegardlessOfFilename(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := "abcdef123456"
	// The artifact on disk carries a DIFFERENT filename than the ledger recorded.
	onDisk := sha + "-review-retry-r3-" + sha + ".md"
	if err := os.WriteFile(filepath.Join(inbox, onDisk), []byte("verdict"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "review-ledger.jsonl")
	row := `{"event":"verdict","sha":"` + sha + `0000000000000000000000000000","reviewer":"r1","verdict":"PASS","artifact":"` + sha + `-review-original-name.md"}`
	if err := os.WriteFile(ledger, []byte(row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := sweepUningestedArtifacts(dir, ledger)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a verdicted SHA must count as ingested even under a different filename, got %v", got)
	}
}

// A SHA with no verdict row anywhere is still a genuine backlog item, so the fix
// cannot be satisfied by reporting an empty sweep.
func TestSweepStillReportsAnArtifactWithNoVerdictRow(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "fedcba654321-review-never-ingested.md"
	if err := os.WriteFile(filepath.Join(inbox, name), []byte("verdict"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "review-ledger.jsonl")
	if err := os.WriteFile(ledger, []byte(`{"event":"verdict","sha":"1111111111111111","reviewer":"r1","verdict":"PASS"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sweepUningestedArtifacts(dir, ledger)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("an artifact with no verdict row is a real backlog item, got %v", got)
	}
}

// FAC-647: a hedged family claim ("openai -- inferred, not fabricated: ...") is
// honest provenance. It routes to `unrecorded` rather than being accepted as
// proven, because the gate proves disjointness and an inference is not a proof.
func TestHedgedFamilyClaimIsHonestlyUnrecordedNotRefused(t *testing.T) {
	raw := "openai — inferred, not fabricated: both new describe-block additions name their tests `security-sentinel: ...`. No stronger per-commit attribution exists."
	family, honest := honestlyUnrecordedFamily(raw)
	if !honest {
		t.Fatal("a hedged family claim must be admitted as honestly unrecorded, not refused as unprovable")
	}
	if family != reviewledger.FamilyUnrecorded {
		t.Fatalf("a hedged claim must NOT be upgraded to a proven family, got %q", family)
	}
}

// A clean bare family must not be laundered into `unrecorded`.
func TestBareFamilyIsNotTreatedAsUnrecorded(t *testing.T) {
	for _, f := range []string{"openai", "anthropic", "xai", "google"} {
		if _, honest := honestlyUnrecordedFamily(f); honest {
			t.Errorf("bare family %q must resolve normally, not as unrecorded", f)
		}
	}
	// And a near-miss typo is still refused (the FAC-628 guarantee).
	if _, honest := honestlyUnrecordedFamily("anthropc"); honest {
		t.Error("a typo must not be accepted as honest provenance")
	}
}

// FAC-651: the callback bus already existed (mail.Callback, CallbackComplete,
// PostCallback with effect-level dedupe, pulse's ConsumeCallback/ack loop) and
// `herd shot` used all of it, while the review path used none. Every stage after
// a verdict was therefore discovered by polling: a verdict admitted at 21:04
// could sit until the next beat purely because nothing announced it.
func TestReviewAdmissionPostsACompletionCallback(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)

	postReviewCompleteCallback(root, sha, "feat/thing", "pool-03", "PASS")

	cbs, err := mail.NewMailbox(mail.CallbackMailPath(root)).DrainCallbacks()
	if err != nil {
		t.Fatalf("drain callbacks: %v", err)
	}
	if len(cbs) != 1 {
		t.Fatalf("an admitted verdict must announce itself exactly once, got %d", len(cbs))
	}
	got := cbs[0]
	if got.Kind != mail.CallbackComplete {
		t.Errorf("kind = %q, want complete", got.Kind)
	}
	if got.SHA != sha {
		t.Errorf("the exact candidate SHA must ride the event: got %q", got.SHA)
	}
	if got.Ref != "feat/thing" {
		t.Errorf("ref = %q, want the branch so the consumer can act without a lookup", got.Ref)
	}
}

// The ingest sweep is idempotent and re-runs constantly, so re-announcing the
// same admitted verdict must not enqueue a second event.
func TestReviewCompletionCallbackIsDedupedAcrossReIngest(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("b", 40)

	for i := 0; i < 3; i++ {
		postReviewCompleteCallback(root, sha, "feat/thing", "pool-03", "PASS")
	}
	cbs, err := mail.NewMailbox(mail.CallbackMailPath(root)).DrainCallbacks()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(cbs) != 1 {
		t.Fatalf("three re-ingests of one verdict must yield ONE event, got %d", len(cbs))
	}
}

// A verdict with no SHA is not an announceable event, and must not post a
// callback naming nothing.
func TestReviewCompletionCallbackRefusesAnEmptySHA(t *testing.T) {
	root := t.TempDir()
	postReviewCompleteCallback(root, "   ", "feat/thing", "pool-03", "PASS")
	cbs, _ := mail.NewMailbox(mail.CallbackMailPath(root)).DrainCallbacks()
	if len(cbs) != 0 {
		t.Fatalf("an empty SHA must not announce anything, got %d", len(cbs))
	}
}

// FAC-657: the record and verdict rows must name the SAME task, because
// Ledger.Admit compares them for equality. They were written from different
// sources -- record.Task from the BRANCH, verdict.Task from the CARD REF -- so
// on the live ledger 0 of 1027 SHAs had them equal and 726 verdict rows carried
// none at all. The comparison could never succeed.
//
// FAC-578: agreeing on a branch is still an unclosable identity. Both rows
// share one function; that function returns only a closeable card ref.
func TestIngestTaskIdentityIsSharedByBothRows(t *testing.T) {
	got := ingestTaskIdentityFor("CHA-2796", "feat/some-branch")
	if got != "CHA-2796" {
		t.Errorf("the card ref is the task identity, got %q", got)
	}
	if got := ingestTaskIdentityFor("", "feat/some-branch"); got != "" {
		t.Errorf("a branch must never become the task identity, got %q", got)
	}
	if got := ingestTaskIdentityFor("standing/api-crusader", "standing/api-crusader"); got != "" {
		t.Errorf("a standing lane name is not closable, got %q", got)
	}
	if got := ingestTaskIdentityFor("fix/cha-2120-telegram", "fix/cha-2120-telegram"); got != "" {
		t.Errorf("a card-shaped token inside a branch must not count, got %q", got)
	}
}

func TestIngestRefusesBranchAsTaskByArtifactName(t *testing.T) {
	err := reviewledger.RequireCloseableCardRef("", "artifact only-branch.md task")
	if err == nil {
		t.Fatal("empty task must refuse")
	}
	if !strings.Contains(err.Error(), "artifact only-branch.md task") || !strings.Contains(err.Error(), "FAC-578") {
		t.Fatalf("refusal must name the artifact, got %v", err)
	}
	err = reviewledger.RequireCloseableCardRef("wt/chain-indexer", "artifact only-branch.md task")
	if err == nil || !strings.Contains(err.Error(), `artifact only-branch.md task "wt/chain-indexer"`) {
		t.Fatalf("refusal must name the bad value, got %v", err)
	}
}

// FAC-675: a pool slot was leased at launch and released only when the launching
// command returned. A reviewer that outlives its launcher -- the normal case,
// since the launcher exits as soon as the agent starts -- left the lease held
// with nothing running behind it. The pool then reported itself saturated while
// slots sat idle, and dispatch waited on capacity that existed.
func TestPoolReclaimIsSilentWithoutARecordedLease(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(root, "ledger.jsonl"))
	if err := os.WriteFile(filepath.Join(root, "ledger.jsonl"),
		[]byte(`{"event":"record","sha":"`+strings.Repeat("a", 40)+`","reviewer":"r1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A launch predating FAC-656 has no recorded lease. There is nothing to
	// reclaim and nothing to report: it must not error, and must not guess at a
	// slot from the surface path, because a guess could release a slot another
	// reviewer has since taken.
	reclaimReviewPoolSlotFor(strings.Repeat("a", 40))
}

// An empty sha identifies nothing and must never trigger a release.
func TestPoolReclaimRefusesAnEmptySHA(t *testing.T) {
	t.Setenv("HERD_ROOT", t.TempDir())
	reclaimReviewPoolSlotFor("   ")
}
