package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// FAC-145: PostCallback's dedupe is atomic under the mailbox transaction —
// concurrent posters of the same DedupeID (separate handles, so the race is
// at the file level) land exactly one envelope.
func TestPostCallback_ConcurrentDedupeAtMostOnce(t *testing.T) {
	mailFile := filepath.Join(t.TempDir(), "mail.jsonl")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mb := NewMailbox(mailFile)
			if _, err := mb.PostCallback("coordinator", Callback{
				Ref: "FAC-1", Kind: CallbackComplete, SHA: "abc123",
				Repo: "herdforge", DedupeID: "dedupe-x",
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(mailFile)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, "dedupe-x") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 deduped envelope, got %d:\n%s", count, data)
	}

	// A later replay with the same id returns the existing envelope.
	mb := NewMailbox(mailFile)
	env, err := mb.PostCallback("coordinator", Callback{
		Ref: "FAC-1", Kind: CallbackComplete, SHA: "abc123",
		Repo: "herdforge", DedupeID: "dedupe-x",
	})
	if err != nil || env == nil {
		t.Fatalf("replay must return the existing envelope: %v", err)
	}
	after, err := os.ReadFile(mailFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(data) {
		t.Fatal("replay must not append")
	}
}

// FAC-145 supersession: the latest verdict on the bus wins — a REJECTED
// posted after an APPROVED vetoes it, and only a FRESH later APPROVED
// (distinct effect identity) restores approval.
func TestEffectiveVerdict_Supersession(t *testing.T) {
	mailFile := filepath.Join(t.TempDir(), "mail.jsonl")
	mb := NewMailbox(mailFile)
	post := func(kind CallbackKind, gen int64, verdict string) {
		t.Helper()
		if _, err := mb.PostCallback("coordinator", Callback{
			Ref: "FAC-1", Kind: kind, SHA: "cafe", Repo: "herdforge",
			LeaseGeneration: gen, SenderRole: "reviewer",
			DedupeID: VerdictEffectID("herdforge:FAC-1:cafe:gen" + fmt.Sprint(gen) + ":claim:1:" + verdict),
		}); err != nil {
			t.Fatal(err)
		}
	}

	post(CallbackComplete, 1, "APPROVED")
	eff, found, err := mb.EffectiveVerdict("herdforge", "FAC-1", "cafe")
	if err != nil || !found || eff.Kind != CallbackComplete {
		t.Fatalf("initial APPROVED must be effective: %+v %v %v", eff, found, err)
	}

	post(CallbackBlocked, 1, "REJECTED")
	eff, _, err = mb.EffectiveVerdict("herdforge", "FAC-1", "cafe")
	if err != nil || eff.Kind != CallbackBlocked {
		t.Fatalf("later REJECTED must veto: %+v %v", eff, err)
	}

	// A replay of the ORIGINAL approval dedupes and cannot undo the veto.
	post(CallbackComplete, 1, "APPROVED")
	eff, _, err = mb.EffectiveVerdict("herdforge", "FAC-1", "cafe")
	if err != nil || eff.Kind != CallbackBlocked {
		t.Fatalf("replayed old APPROVED must not undo the veto: %+v %v", eff, err)
	}

	// A FRESH admissible approval (new generation = new effect) wins again.
	post(CallbackComplete, 2, "APPROVED")
	eff, _, err = mb.EffectiveVerdict("herdforge", "FAC-1", "cafe")
	if err != nil || eff.Kind != CallbackComplete {
		t.Fatalf("fresh APPROVED must supersede the veto: %+v %v", eff, err)
	}
}

// FAC-145 (blocker 5): a corrupt callback body must never be silently
// skipped — it could BE the delivered marker or the veto, so verdict-state
// decisions fail closed instead of losing authority records.
func TestVerdictState_FailsClosedOnCorruptBody(t *testing.T) {
	mailFile := filepath.Join(t.TempDir(), "mail.jsonl")
	mb := NewMailbox(mailFile)
	if _, err := mb.PostCallback("reviewer", Callback{
		Ref: "FAC-1", Kind: CallbackComplete, SHA: "cafe", Repo: "herdforge",
		LeaseGeneration: 1, SenderRole: "reviewer",
		DedupeID: VerdictEffectID("herdforge:FAC-1:cafe:gen1:claim:1:APPROVED"),
	}); err != nil {
		t.Fatal(err)
	}
	// A corrupt callback envelope lands on the same bus.
	corrupt := `{"id":"x","seq":99,"sender":"reviewer","recipient":"coordinator","subject":"blocked: FAC-1","body":"{not json","read":false,"timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	f, err := os.OpenFile(mailFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(corrupt); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, _, err := mb.EffectiveVerdict("herdforge", "FAC-1", "cafe"); err == nil {
		t.Fatal("EffectiveVerdict must fail closed on a corrupt callback body")
	}
	if _, _, err := mb.HasDeliveredVerdict(VerdictEffectID("herdforge:FAC-1:cafe:gen1:claim:1:APPROVED")); err == nil {
		t.Fatal("HasDeliveredVerdict must fail closed on a corrupt callback body")
	}
}
