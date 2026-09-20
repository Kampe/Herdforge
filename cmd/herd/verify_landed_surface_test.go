package main

import (
	"context"
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

// carrierAt is a lookup that ANSWERED: dir, or "" for a branch no worktree
// holds. It is deliberately distinct from carrierLookupFailed below, because
// those two used to be the same value.
func carrierAt(dir string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return dir, nil }
}

// carrierLookupFailed is a lookup that COULD NOT ANSWER.
func carrierLookupFailed(err error) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return "", err }
}

// A live carrier is used exactly as before. This is the no-change half, and it
// must stay true or the fix would have relaxed the active/dirty protection.
func TestResolveVerifyLandedSurfaceUsesALiveCarrierUnchanged(t *testing.T) {
	dir := surfaceRepo(t)
	surface, err := resolveVerifyLandedSurface(context.Background(), "fix/x", verifyLandedBinding{},
		carrierAt("/carrier/dir"), rootOf(dir))
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
	surface, err := resolveVerifyLandedSurface(context.Background(), "fix/x", verifyLandedBinding{},
		carrierAt(""), rootOf(dir))
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

// A retired carrier WITH an explicit pinned candidate may use the invoking
// checkout. Selection does not check that the object is present here, and no
// longer claims to: that is proved by the bounded gate proof. See
// TestVerifyLandedGateRefusesAnAbsentPin.
func TestResolveVerifyLandedSurfaceAcceptsAPinnedCandidate(t *testing.T) {
	dir := surfaceRepo(t)
	candidate := surfaceCommit(t, dir, "a.txt", "one\n")

	surface, err := resolveVerifyLandedSurface(context.Background(), "fix/x", verifyLandedBinding{Candidate: candidate},
		carrierAt(""), rootOf(dir))
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

// FOREIGN REPOSITORY: selection no longer decides this. It returns the invoking
// root without looking at the repository at all, and the bounded gate proof
// refuses it, because that is the step that resolves the object it will
// actually prove. The refusal itself is asserted in
// TestVerifyLandedGateRefusesAnAbsentPin; this pins the SELECTION half of the
// contract so the move cannot silently become an acceptance.
func TestResolveVerifyLandedSurfaceSelectsWithoutProvingTheRepository(t *testing.T) {
	reviewed := surfaceRepo(t)
	candidate := surfaceCommit(t, reviewed, "a.txt", "one\n")

	foreign := surfaceRepo(t)
	surfaceCommit(t, foreign, "z.txt", "unrelated\n")

	surface, err := resolveVerifyLandedSurface(context.Background(), "fix/x", verifyLandedBinding{Candidate: candidate},
		carrierAt(""), rootOf(foreign))
	if err != nil {
		t.Fatalf("selection ran a check of its own: %v", err)
	}
	if surface.Dir != foreign || !surface.CarrierRetired {
		t.Fatalf("selection did not return the invoking root: %+v", surface)
	}
	if surface.PinnedCandidate != candidate {
		t.Fatal("selection lost the pin that the proof must be bound to")
	}
}

// pinnedCandidateFor never reaches for a branch head.
func TestPinnedCandidateForPrefersExplicitAndNeverUsesABranchHead(t *testing.T) {
	if got, err := pinnedCandidateFor(context.Background(), verifyLandedBinding{Candidate: "  abc123  "}); err != nil || got != "abc123" {
		t.Fatalf("explicit candidate = %q: %v", got, err)
	}
	if got, err := pinnedCandidateFor(context.Background(), verifyLandedBinding{}); err != nil || got != "" {
		t.Fatalf("an unpinned binding produced %q (%v); only --candidate or an admitted PASS may pin", got, err)
	}
}

// A FAILED LOOKUP IS NOT AN ABSENT CARRIER (review b70054a6 / BQ advisory).
//
// The lookup used to report every failure as the empty string, and "" arrives
// here as "no worktree carries this branch" -- which authorises the invoking
// checkout to stand in as the proof surface. An unreadable worktree list says
// nothing about whether a live lane exists, so selection must refuse instead of
// answering. A pinned candidate does NOT rescue it: the pin authorises an
// object, not a scope.
func TestResolveVerifyLandedSurfaceRefusesAFailedCarrierLookup(t *testing.T) {
	dir := surfaceRepo(t)
	candidate := surfaceCommit(t, dir, "a.txt", "one\n")
	boom := errors.New("worktree list could not run")

	surface, err := resolveVerifyLandedSurface(context.Background(), "fix/x",
		verifyLandedBinding{Candidate: candidate}, carrierLookupFailed(boom), rootOf(dir))
	if err == nil {
		t.Fatalf("a failed carrier lookup authorised %q as a proof surface", surface.Dir)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the lookup failure preserved through errors.Is", err)
	}
	if errors.Is(err, errRetiredCarrierUnpinned) {
		t.Fatalf("a failed lookup was reported as an absent carrier: %v", err)
	}
	if surface.Dir != "" || surface.CarrierRetired {
		t.Fatalf("a refusal returned a surface: %+v", surface)
	}
}

// The production lookup itself separates the two answers, against real git.
// Without this the distinction above could hold in the selector while the
// shipped lookup still reported both cases identically.
func TestWorktreeForBranchSeparatesAnAbsentCarrierFromAFailedLookup(t *testing.T) {
	dir := surfaceRepo(t)
	surfaceCommit(t, dir, "a.txt", "one\n")
	t.Chdir(dir)

	got, err := worktreeForBranch(context.Background(), "branch/nobody/holds")
	if err != nil {
		t.Fatalf("an absent carrier must be an ANSWER, not a failure: %v", err)
	}
	if got != "" {
		t.Fatalf("dir = %q, want the empty answer for a branch no worktree holds", got)
	}

	// A directory that is not a repository at all: the lookup cannot answer.
	t.Chdir(t.TempDir())
	got, err = worktreeForBranch(context.Background(), "branch/nobody/holds")
	if err == nil {
		t.Fatalf("a lookup that could not run reported %q as an absent carrier", got)
	}
	if got != "" {
		t.Fatalf("a failed lookup returned a directory: %q", got)
	}
}
