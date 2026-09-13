package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real git fixtures. Identity and signing are configured locally so these pass
// on a clean CI machine and on a laptop with global signing turned on.
func surfaceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	surfaceGit(t, dir, "init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.name", "herd test"},
		{"user.email", "herd@example.invalid"},
		{"commit.gpgsign", "false"},
		{"gc.auto", "0"},
	} {
		surfaceGit(t, dir, "config", kv[0], kv[1])
	}
	return dir
}

func surfaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func surfaceCommit(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	surfaceGit(t, dir, "add", name)
	surfaceGit(t, dir, "commit", "-q", "-m", "add "+name)
	return surfaceGit(t, dir, "rev-parse", "HEAD")
}

func rootOf(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// A live carrier is used exactly as before. This is the no-change half, and it
// must stay true or the fix would have relaxed the active/dirty protection.
func TestResolveVerifyLandedSurfaceUsesALiveCarrierUnchanged(t *testing.T) {
	dir := surfaceRepo(t)
	surface, err := resolveVerifyLandedSurface("fix/x", verifyLandedBinding{},
		func(string) string { return "/carrier/dir" }, rootOf(dir))
	if err != nil {
		t.Fatalf("live carrier: %v", err)
	}
	if surface.Dir != "/carrier/dir" {
		t.Fatalf("Dir = %q, want the carrier", surface.Dir)
	}
	if surface.CarrierRetired {
		t.Fatal("a live carrier was reported retired")
	}
	if surface.PinnedCandidate != "" {
		t.Fatal("a live carrier required a pinned candidate; that is a behaviour change")
	}
}

// REGRESSION (FAC-831), the refusal half: a retired carrier with NOTHING
// pinning the candidate must refuse. It must not fall through to the invoking
// checkout and let a containment check reject an unrelated HEAD incidentally —
// in a repository whose HEAD is not yet on origin/main that check would not
// fire at all.
func TestResolveVerifyLandedSurfaceRefusesRetiredCarrierWithoutAPin(t *testing.T) {
	dir := surfaceRepo(t)
	head := surfaceCommit(t, dir, "a.txt", "one\n")
	if head == "" {
		t.Fatal("fixture produced no HEAD")
	}
	// The invoking checkout HAS a perfectly good HEAD. It is still refused,
	// because nothing ties that HEAD to the reviewed candidate.
	surface, err := resolveVerifyLandedSurface("fix/x", verifyLandedBinding{},
		func(string) string { return "" }, rootOf(dir))
	if err == nil {
		t.Fatalf("used %q as a proof surface with no pinned candidate", surface.Dir)
	}
	if !errors.Is(err, errRetiredCarrierUnpinned) {
		t.Fatalf("err = %v, want errRetiredCarrierUnpinned", err)
	}
	if surface.Dir != "" {
		t.Fatalf("a refusal returned a surface: %q", surface.Dir)
	}
}

// A retired carrier WITH an explicit pinned candidate present in this
// repository may use the invoking checkout.
func TestResolveVerifyLandedSurfaceAcceptsAPinnedCandidateInThisRepo(t *testing.T) {
	dir := surfaceRepo(t)
	candidate := surfaceCommit(t, dir, "a.txt", "one\n")

	surface, err := resolveVerifyLandedSurface("fix/x", verifyLandedBinding{Candidate: candidate},
		func(string) string { return "" }, rootOf(dir))
	if err != nil {
		t.Fatalf("pinned candidate refused: %v", err)
	}
	if surface.Dir != dir {
		t.Fatalf("Dir = %q, want the invoking root %q", surface.Dir, dir)
	}
	if !surface.CarrierRetired {
		t.Fatal("the retired carrier was not reported")
	}
	if surface.PinnedCandidate != candidate {
		t.Fatalf("PinnedCandidate = %s, want %s", shortSHA12(surface.PinnedCandidate), shortSHA12(candidate))
	}
}

// FOREIGN REPOSITORY: the pin names an object this repository does not hold, so
// the invoking checkout is not the one the candidate was reviewed in.
func TestResolveVerifyLandedSurfaceRefusesAForeignRepository(t *testing.T) {
	reviewed := surfaceRepo(t)
	candidate := surfaceCommit(t, reviewed, "a.txt", "one\n")

	foreign := surfaceRepo(t)
	surfaceCommit(t, foreign, "z.txt", "unrelated\n")

	surface, err := resolveVerifyLandedSurface("fix/x", verifyLandedBinding{Candidate: candidate},
		func(string) string { return "" }, rootOf(foreign))
	if err == nil {
		t.Fatalf("proved a landing in a repository that does not hold the candidate: %q", surface.Dir)
	}
	if !strings.Contains(err.Error(), "not present in the invoking repository") {
		t.Fatalf("refusal reason = %v", err)
	}
}

// requireObjectPresent must demand a COMMIT. A tree or blob id resolves in git
// but is not a candidate.
func TestRequireObjectPresentDemandsACommit(t *testing.T) {
	dir := surfaceRepo(t)
	head := surfaceCommit(t, dir, "a.txt", "one\n")
	tree := surfaceGit(t, dir, "rev-parse", head+"^{tree}")

	if err := requireObjectPresent(dir, head); err != nil {
		t.Fatalf("a real commit was refused: %v", err)
	}
	if err := requireObjectPresent(dir, tree); err == nil {
		t.Fatal("a tree id was accepted as a candidate commit")
	}
	if err := requireObjectPresent(dir, strings.Repeat("0", 40)); err == nil {
		t.Fatal("an absent object was accepted")
	}
}

// pinnedCandidateFor never reaches for a branch head.
func TestPinnedCandidateForPrefersExplicitAndNeverUsesABranchHead(t *testing.T) {
	if got := pinnedCandidateFor(verifyLandedBinding{Candidate: "  abc123  "}); got != "abc123" {
		t.Fatalf("explicit candidate = %q", got)
	}
	if got := pinnedCandidateFor(verifyLandedBinding{}); got != "" {
		t.Fatalf("an unpinned binding produced %q; only --candidate or an admitted PASS may pin", got)
	}
}
