package reviewledger

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLedger(t *testing.T, lines ...string) (string, string) {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "review-ledger.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, p
}

func TestProvenBuilderFamily_ReturnsRecordedFamilyForExactSHA(t *testing.T) {
	root, p := writeLedger(t,
		`{"event":"record","sha":"aaa","reviewer":"r1","builder_family":"xai"}`,
		`{"event":"record","sha":"bbb","reviewer":"r2","builder_family":"openai"}`)
	l, err := NewReadOnlyReviewLedger(root, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.ProvenBuilderFamily("aaa")
	if err != nil || got != "xai" {
		t.Fatalf("got %q err=%v, want xai", got, err)
	}
}

// A family outside the allowlist is not proof. "codex" is a harness name, and
// admitting it here would let an unadmittable review be dispatched anyway.
func TestProvenBuilderFamily_RejectsNonAllowlistedFamily(t *testing.T) {
	root, p := writeLedger(t, `{"event":"record","sha":"aaa","reviewer":"r1","builder_family":"codex"}`)
	l, _ := NewReadOnlyReviewLedger(root, p)
	got, err := l.ProvenBuilderFamily("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("non-allowlisted family must not count as proven, got %q", got)
	}
}

// A verdict row is not a launch record. Only the launch proves who built it.
func TestProvenBuilderFamily_IgnoresVerdictRows(t *testing.T) {
	root, p := writeLedger(t, `{"event":"verdict","sha":"aaa","reviewer":"r1","builder_family":"xai"}`)
	l, _ := NewReadOnlyReviewLedger(root, p)
	got, _ := l.ProvenBuilderFamily("aaa")
	if got != "" {
		t.Fatalf("a verdict row must not stand in for a launch record, got %q", got)
	}
}

func TestProvenBuilderFamily_UnknownSHAIsUnprovenNotError(t *testing.T) {
	root, p := writeLedger(t, `{"event":"record","sha":"aaa","reviewer":"r1","builder_family":"xai"}`)
	l, _ := NewReadOnlyReviewLedger(root, p)
	got, err := l.ProvenBuilderFamily("zzz")
	if err != nil || got != "" {
		t.Fatalf("got %q err=%v, want unproven with no error", got, err)
	}
}
