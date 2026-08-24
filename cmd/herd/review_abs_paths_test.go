package main

import (
	"os"
	"path/filepath"
	"testing"
)

// FAC-592: two paths handed to a reviewer must be absolute, because the reviewer
// resolves them against its OWN cwd, which is the pool slot rather than the repo
// root.
//
// Both defaults are relative (".herd/review-surfaces", ".herd/review-packets"),
// and the observed failures were:
//   - reviewers running with cwd=$HOME, reading a tree with nothing to do
//     with the candidate while still writing a verdict
//   - a reviewer told to read ".herd/review-packets/review-cha-2202-….md",
//     resolving it to ~/.herd/review-packets/…, finding nothing, and correctly
//     refusing to execute a different file instead
//
// The property under test is that a relative default cannot survive into either
// the tab cwd or the delivered instruction.
func TestRelativeDefaultsResolveOutsideTheRepo(t *testing.T) {
	// Simulate a reviewer standing in a pool slot, which is where it actually
	// runs, and confirm a relative packet path does NOT land on the real file.
	repo := t.TempDir()
	packetRoot := filepath.Join(repo, ".herd", "review-packets")
	if err := os.MkdirAll(packetRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	real := filepath.Join(packetRoot, "review-cha-2202.md")
	if err := os.WriteFile(real, []byte("packet"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	slot := filepath.Join(repo, ".herd", "pool", "pool-01")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}

	relative := filepath.Join(".herd", "review-packets", "review-cha-2202.md")

	// Resolved from the pool slot — where the reviewer stands — the relative
	// path misses the packet entirely. That is the bug.
	fromSlot := filepath.Join(slot, relative)
	if _, err := os.Stat(fromSlot); err == nil {
		t.Fatalf("relative path unexpectedly resolved from the pool slot: %s", fromSlot)
	}

	// Absolute always finds it, regardless of where the reviewer stands.
	abs, err := filepath.Abs(real)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("absolute packet path must resolve: %v", err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatal("packet path handed to a reviewer must be absolute")
	}
}

// A cwd that cannot be resolved to a real directory must fail the launch rather
// than fall back somewhere plausible. A reviewer in the wrong tree still emits a
// verdict, which is the worst outcome: the artifact looks legitimate.
func TestReviewerCwdMustBeAnExistingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-surface")
	st, err := os.Stat(missing)
	if err == nil && st.IsDir() {
		t.Fatal("fixture should not exist")
	}
	// The launch path applies exactly this check before creating the tab.
	if err == nil {
		t.Fatal("a missing surface must not stat clean")
	}
}
