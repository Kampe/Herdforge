package candidateindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
)

// FAC-750 reproduction, replaying FAC-746's exact live event sequence.
//
// A reviewer's verdict is written as a NON-CONSUMABLE intent (kind=blocked)
// BEFORE provider delivery, so a half-published verdict can never read as an
// approval (cmd/herd/main.go: "an intent alone can NEVER read as an approval").
// The index files ANY blocked callback as BlockedVetoVerdict, so a reviewer's
// APPROVAL is indistinguishable from a reviewer VETO. On the live fleet this is
// why b3da4492845e sat "vetoed" while three reviewers had approved it, and why
// origin/main went unmoved for two days.
//
// The detail text is stamped "verdict intent (undelivered)" at write time and
// never updated, so it reads like a live delivery status. It is not one: every
// FAC-746 verdict WAS delivered to the provider, signed, within ~200ms. Only
// the ledger ingest is missing. Do not restate this as a delivery failure.
//
// Live sequence for the winning SHA (control-mail.jsonl):
//
//	seq714 complete gen23 worker
//	seq715 complete gen23 shot
//	seq716 blocked  gen2  reviewer  <- "verdict intent (undelivered): ... APPROVED"
//
// The block is the LAST event, so the unblock condition
// (env.Sequence > block.sequence) is unsatisfiable and it strands permanently.
func TestVerdictIntentDoesNotReadAsVeto(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "control-mail.jsonl")
	f, err := os.Create(mailPath)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "b3da4492845e01b0911a291810f3eea48778d1ce"

	write := func(sequence int64, sender string, cb mail.Callback) {
		t.Helper()
		body, mErr := json.Marshal(cb)
		if mErr != nil {
			t.Fatal(mErr)
		}
		if eErr := json.NewEncoder(f).Encode(mail.Envelope{
			ID:        fmt.Sprintf("callback-%d", sequence),
			Sequence:  sequence,
			Sender:    sender,
			Recipient: mail.CoordinatorInbox,
			Subject:   string(cb.Kind) + ": FAC-746",
			Body:      string(body),
			Timestamp: time.Unix(sequence, 0).UTC(),
		}); eErr != nil {
			t.Fatal(eErr)
		}
	}

	write(714, "worker", mail.Callback{
		Ref: "FAC-746", Kind: mail.CallbackComplete, SHA: sha,
		LeaseGeneration: 23, SenderRole: "worker",
	})
	// The reviewer APPROVED. Only the non-consumable write-ahead intent reaches
	// the mailbox, and it is kind=blocked; the delivered effect lives on the
	// provider, which this index never reads.
	write(716, "reviewer", mail.Callback{
		Ref: "FAC-746", Kind: mail.CallbackBlocked, SHA: sha,
		Detail: "verdict intent (undelivered): REVIEW VERDICT FAC-746: APPROVED " +
			"candidate=" + sha + " base=b42b69763598436c29d49e7922b27f011ae168a5 " +
			"lease=claim:282 lease_gen=2 reviewer-bound (FAC-145)",
		LeaseGeneration: 2, SenderRole: "reviewer",
		DedupeID: mail.VerdictIntentID("herdforge:FAC-746:" + sha + ":gen2:claim:282:APPROVED"),
	})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cands, err := New(IndexOptions{RepoRoot: dir, MailPath: mailPath}).BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(cands), cands)
	}
	c := cands[0]

	for _, r := range c.BlockedReasons {
		if r == BlockedVetoVerdict {
			t.Fatalf("an APPROVED verdict whose delivery failed is recorded as a reviewer veto (%s).\n"+
				"state=%s reasons=%v\nevidence=%v\n"+
				"The index cannot distinguish 'reviewer vetoed this' from 'reviewer approved this "+
				"and delivery failed' — both are kind=blocked. This is FAC-750.",
				BlockedVetoVerdict, c.State, c.BlockedReasons, c.BlockedEvidence)
		}
	}
	if !hasReason(c.BlockedReasons, BlockedVerdictIntentOnly) {
		t.Fatalf("want %s, got reasons=%v", BlockedVerdictIntentOnly, c.BlockedReasons)
	}
	// It must still BLOCK: an un-ingested verdict is not a pass.
	if c.State != StateBlocked {
		t.Fatalf("state=%s, want blocked — an un-ingested verdict must never read as eligible", c.State)
	}
	var said bool
	for _, e := range c.BlockedEvidence {
		if strings.Contains(e, "NOT a reviewer veto") {
			said = true
		}
	}
	if !said {
		t.Fatalf("evidence must say the block is an un-ingested intent, not a veto: %v", c.BlockedEvidence)
	}
}

// The fix must not silence genuine vetoes. A blocked callback that is NOT a
// verdict intent is still a veto. Without this, classifying everything as
// intent-only would pass the test above.
func TestGenuineVetoStillReadsAsVeto(t *testing.T) {
	dir := t.TempDir()
	mailPath := filepath.Join(dir, "control-mail.jsonl")
	f, err := os.Create(mailPath)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "ad2f69e015c2000000000000000000000000abcd"
	body, _ := json.Marshal(mail.Callback{
		Ref: "FAC-746", Kind: mail.CallbackBlocked, SHA: sha,
		Detail: "managed verify interrupted; no full-suite test receipt", LeaseGeneration: 1,
	})
	if err := json.NewEncoder(f).Encode(mail.Envelope{
		ID: "cb-1", Sequence: 1, Sender: "worker", Recipient: mail.CoordinatorInbox,
		Subject: "blocked: FAC-746", Body: string(body), Timestamp: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cands, err := New(IndexOptions{RepoRoot: dir, MailPath: mailPath}).BuildIndex(context.Background())
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	if !hasReason(cands[0].BlockedReasons, BlockedVetoVerdict) {
		t.Fatalf("a non-intent blocked callback must still be a veto, got %v", cands[0].BlockedReasons)
	}
}

func hasReason(reasons []BlockedReason, want BlockedReason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
