package mail

import (
	"path/filepath"
	"testing"
)

func newTestMailbox(t *testing.T) *Mailbox {
	t.Helper()
	return NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
}

func TestPostAndDrainCallbacks(t *testing.T) {
	m := newTestMailbox(t)

	if _, err := m.PostCallback("task-fac-64", Callback{Ref: "FAC-64", Kind: CallbackComplete, SHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PostCallback("task-fac-72", Callback{Ref: "FAC-72", Kind: CallbackBlocked, Detail: "needs a decision"}); err != nil {
		t.Fatal(err)
	}
	// A plain non-callback message in the same inbox must be ignored.
	if _, err := m.SendMessage("someone", CoordinatorInbox, "hello", "just a note"); err != nil {
		t.Fatal(err)
	}

	cbs, err := m.DrainCallbacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(cbs) != 2 {
		t.Fatalf("want 2 callbacks (plain msg skipped), got %d: %+v", len(cbs), cbs)
	}
	byRef := map[string]Callback{}
	for _, c := range cbs {
		byRef[c.Ref] = c
	}
	if byRef["FAC-64"].Kind != CallbackComplete || byRef["FAC-64"].SHA != "abc123" {
		t.Fatalf("FAC-64 callback wrong: %+v", byRef["FAC-64"])
	}
	if byRef["FAC-72"].Kind != CallbackBlocked || byRef["FAC-72"].Detail != "needs a decision" {
		t.Fatalf("FAC-72 callback wrong: %+v", byRef["FAC-72"])
	}
}

func TestDrainCallbacks_EmptyInbox(t *testing.T) {
	m := newTestMailbox(t)
	cbs, err := m.DrainCallbacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(cbs) != 0 {
		t.Fatalf("empty inbox must drain nothing, got %d", len(cbs))
	}
}

// TestPostCallback_VerdictMarkerNotSwallowedByCompletion proves the delivered
// verdict's effect identity survives an unrelated completion with the same
// task ref and lease generation: a DedupeID-carrying callback is deduped by
// that identity alone, so the consumable verdict marker must actually land
// (R3 round 5, finding 4).
func TestPostCallback_VerdictMarkerNotSwallowedByCompletion(t *testing.T) {
	m := newTestMailbox(t)
	verdictID := VerdictEffectID("herdforge:FAC-1:cafe1234:gen1:claim:9:APPROVED")

	// An unrelated completion for the same ref and fence is on the bus first.
	first, err := m.PostCallback("task-fac-1", Callback{
		Ref: "FAC-1", Kind: CallbackComplete, SHA: "cafe1234", Repo: "herdforge", LeaseGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The broker's delivered-verdict post must APPEND its own marker, not
	// converge into the unrelated completion.
	second, err := m.PostCallback("task-fac-1", Callback{
		Ref: "FAC-1", Kind: CallbackComplete, SHA: "cafe1234", Repo: "herdforge",
		LeaseGeneration: 1, DedupeID: verdictID, Detail: "verdict APPROVED (FAC-740)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("the verdict-delivered post was swallowed by an unrelated completion with the same ref and generation")
	}
	if _, found, err := m.HasDeliveredVerdict(verdictID); err != nil || !found {
		t.Fatalf("the consumable verdict marker must be on the bus: found=%v err=%v", found, err)
	}

	// Exactly-once holds for the verdict identity itself: a retry converges
	// onto the verdict envelope, and a DIFFERENT verdict identity for the
	// same ref and fence is a collision refusal.
	retry, err := m.PostCallback("task-fac-1", Callback{
		Ref: "FAC-1", Kind: CallbackComplete, SHA: "cafe1234", Repo: "herdforge",
		LeaseGeneration: 1, DedupeID: verdictID, Detail: "verdict APPROVED (FAC-740)",
	})
	if err != nil || retry.ID != second.ID {
		t.Fatalf("verdict retry must converge by identity: env=%v err=%v", retry, err)
	}
}
