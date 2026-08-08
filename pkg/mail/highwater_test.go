package mail

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-145: HighWaterGeneration is the production CurrentGeneration source
// for receipt fencing — it must read the durable consumer state exactly and
// report zero (never an error state) when nothing was acked.
func TestHighWaterGeneration(t *testing.T) {
	mailFile := filepath.Join(t.TempDir(), "mail.jsonl")

	if got := HighWaterGeneration(mailFile, "herdforge", "FAC-1"); got != 0 {
		t.Fatalf("no state yet: got %d, want 0", got)
	}

	state := `{"acked":{"herdforge|FAC-1":{"lease_generation":7,"sequence":12}},"pending":{}}`
	if err := os.WriteFile(mailFile+".callback-state.json", []byte(state), 0644); err != nil {
		t.Fatal(err)
	}
	if got := HighWaterGeneration(mailFile, "herdforge", "FAC-1"); got != 7 {
		t.Fatalf("acked generation: got %d, want 7", got)
	}
	if got := HighWaterGeneration(mailFile, "herdforge", "FAC-2"); got != 0 {
		t.Fatalf("unacked ref: got %d, want 0", got)
	}
	if got := HighWaterGeneration(mailFile, "other-repo", "FAC-1"); got != 0 {
		t.Fatalf("other repo: got %d, want 0", got)
	}

	// Corrupt state degrades to zero rather than blocking the caller with a
	// phantom generation.
	if err := os.WriteFile(mailFile+".callback-state.json", []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := HighWaterGeneration(mailFile, "herdforge", "FAC-1"); got != 0 {
		t.Fatalf("corrupt state: got %d, want 0", got)
	}
}
