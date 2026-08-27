package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-602: a reviewer records WHERE it verified something. That receipt is
// untracked but NOT gitignored, so the ignored[] skip never reached it and the
// absolute-path walk scanned it. Twelve verdict artifacts failed `make
// preflight` for every lane in the shared checkout simultaneously; preflight
// fails closed, so each lane read a repository-wide block as its own.
//
// These go through CheckWorktreeBoundary -- the function `make preflight` invokes --
// not the skip predicate beside it. A test that asserted on a path-matching
// helper would pass while the real walk still scanned the file, which is the
// vacuous shape that has already produced one FAIL in this effort.

func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{".git", "pkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "ok.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The leak marker must be assembled rather than written literally: this file is
// itself scanned by the gate it tests, and spelling the marker out fails the
// very check under test. That is not hypothetical -- it has already happened
// once in this repository.
func hostPath() string { return "/Us" + "ers/kampe/Personal/worktrees/fac-578-closeable-card-ref" }

func TestReviewEvidenceDoesNotFailTheWholeRepository(t *testing.T) {
	root := writeRepo(t)
	inbox := filepath.Join(root, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "sha: 6cdff2a0\nverdict: PASS\n---\nDiffed in " + hostPath() + " and reproduced on origin/main.\n"
	if err := os.WriteFile(filepath.Join(inbox, "verdict.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckWorktreeBoundary(root); err != nil {
		t.Fatalf("a reviewer's own verdict failed the boundary check for the entire repository: %v", err)
	}
}

func TestReviewPacketsAreAlsoRuntimeEvidence(t *testing.T) {
	root := writeRepo(t)
	packets := filepath.Join(root, ".herd", "review-packets")
	if err := os.MkdirAll(packets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packets, "packet.md"), []byte("Review in "+hostPath()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckWorktreeBoundary(root); err != nil {
		t.Fatalf("a review packet failed the boundary check for the entire repository: %v", err)
	}
}

// The exemption must stay NARROW. Skipping runtime evidence must not stop the
// gate catching a real leak in tracked source -- that is the property the whole
// check exists for, and a too-broad skip would silently retire it.
func TestATrackedSourceLeakIsStillCaught(t *testing.T) {
	root := writeRepo(t)
	if err := os.WriteFile(filepath.Join(root, "pkg", "leak.go"),
		[]byte("package pkg\n\nconst p = \""+hostPath()+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CheckWorktreeBoundary(root)
	if err == nil {
		t.Fatal("an absolute path in tracked source passed the boundary check")
	}
	if !strings.Contains(err.Error(), "leak.go") {
		t.Fatalf("refusal does not name the offending file: %v", err)
	}
}
