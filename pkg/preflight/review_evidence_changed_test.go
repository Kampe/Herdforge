package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-606: FAC-602 exempted review evidence in the WalkDir branch, and its
// tests passed, and the exemption still did nothing.
//
// checkFile has two callers: the changed-paths loop (paths != nil) and the full
// WalkDir (paths == nil). The directory skips live only in the walk. `herd
// preflight` calls CheckWorktreeBoundaryChanged -> the changed-paths loop, so
// the exemption was never on the path an operator triggers.
//
// FAC-602's tests used a bare t.TempDir() with no git history, which falls
// through to the walk. They exercised the branch the fix was in rather than the
// branch the CLI uses, so the tests and the fix agreed with each other and
// neither agreed with the shipped behaviour. The failure survived a merge.
//
// These drive checkWorktreeBoundaryFiles with an EXPLICIT non-nil path list --
// the changed-paths branch itself. If the exemption ever moves back out of
// checkFile into a walk-only skip, these go red.

func evidenceRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{".herd/review/inbox", ".herd/review-packets", "pkg"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Assembled, not literal: this file is scanned by the gate it tests.
func leakPath() string { return "/Us" + "ers/kampe/Personal/worktrees/fac-578-closeable-card-ref" }

func TestChangedPathsBranchExemptsReviewEvidence(t *testing.T) {
	root := evidenceRepo(t)
	verdict := ".herd/review/inbox/verdict.md"
	if err := os.WriteFile(filepath.Join(root, verdict),
		[]byte("verdict: PASS\n---\nDiffed in "+leakPath()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// paths != nil is the branch `herd preflight` takes.
	if err := checkWorktreeBoundaryFiles(root, []string{verdict}, nil); err != nil {
		t.Fatalf("review evidence failed the CHANGED-PATHS branch, which is the one the CLI uses: %v", err)
	}
}

func TestChangedPathsBranchExemptsReviewPackets(t *testing.T) {
	root := evidenceRepo(t)
	packet := ".herd/review-packets/packet.md"
	if err := os.WriteFile(filepath.Join(root, packet), []byte("Reviewed in "+leakPath()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := checkWorktreeBoundaryFiles(root, []string{packet}, nil); err != nil {
		t.Fatalf("review packet failed the CHANGED-PATHS branch: %v", err)
	}
}

// Narrowness, on the same branch. Exempting evidence must not stop the gate
// catching a real leak in tracked source -- otherwise the exemption silently
// retires the check it is carving out of.
func TestChangedPathsBranchStillCatchesTrackedSourceLeaks(t *testing.T) {
	root := evidenceRepo(t)
	src := "pkg/leak.go"
	if err := os.WriteFile(filepath.Join(root, src),
		[]byte("package pkg\n\nconst p = \""+leakPath()+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := checkWorktreeBoundaryFiles(root, []string{src}, nil)
	if err == nil {
		t.Fatal("an absolute path in tracked source passed the changed-paths branch")
	}
	if !strings.Contains(err.Error(), "leak.go") {
		t.Fatalf("refusal does not name the offending file: %v", err)
	}
}
