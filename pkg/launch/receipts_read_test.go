package launch

import (
	"path/filepath"
	"testing"
)

// FAC-646: DefaultReceiptPath is cwd-relative, the third instance of the FAC-643
// class in this tree (after the review ledger and the pulse inbox sweep). A
// caller that already knows the repository root was still reading whichever
// receipt log sat under the process cwd: measured from the Herdforge checkout it
// returned that repo's 12 receipts instead of the target project's, so builder
// family resolved from the wrong fleet's launches, or not at all.
func TestReceiptPathForAnchorsOnTheGivenRoot(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	got := ReceiptPathFor("/project/root")
	want := filepath.Join("/project/root", ".herd", "launch-receipts.jsonl")
	if got != want {
		t.Fatalf("ReceiptPathFor = %q, want %q", got, want)
	}
	if got == DefaultReceiptPath() {
		t.Fatal("a root-anchored path must differ from the cwd-relative default")
	}
}

// An explicit override still wins, and an empty root still falls back rather
// than producing a path rooted at "/".
func TestReceiptPathForOverrideAndEmptyRoot(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "/explicit/receipts.jsonl")
	if got := ReceiptPathFor("/project/root"); got != "/explicit/receipts.jsonl" {
		t.Fatalf("override must win, got %q", got)
	}
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	if got := ReceiptPathFor("  "); got != DefaultReceiptPath() {
		t.Fatalf("empty root must fall back to the default, got %q", got)
	}
}
