package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-626: `herd review --pool --no-launch --sha <candidate>` reported a
// surface ready whose NAME embedded the candidate SHA but whose CONTENT
// (through the pool-slot symlink) was trunk. A reviewer dispatched there
// reviews the wrong commit and produces a plausible, fully-formed verdict
// attributed to the candidate -- not a missing review, a FABRICATED one,
// admitted as canonical cross-family evidence. Existence of the symlink was
// treated as agreement with the candidate; nothing re-read HEAD through it.

// staleSurfaceFixture builds a tiny repo with two distinct commits and a
// review-surface symlink pointing at a worktree checked out to the WRONG one
// -- the exact shape reported live (symlink name embeds one SHA, resolved
// content is a different commit).
func staleSurfaceFixture(t *testing.T) (surface, wantSHA, actualHead, target string) {
	t.Helper()
	base := t.TempDir()

	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "candidate commit")
	wantSHA = gitIn(t, repo, "rev-parse", "HEAD")

	// The "pool slot": a SEPARATE checkout reset to a DIFFERENT commit --
	// trunk having moved on, exactly like the live incident.
	target = filepath.Join(base, "pool", "pool-02")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "worktree", "add", "-q", "--detach", target, "HEAD")
	gitIn(t, target, "commit", "-q", "--allow-empty", "-m", "trunk moved on")
	actualHead = gitIn(t, target, "rev-parse", "HEAD")

	surfaceRoot := filepath.Join(base, "review-surfaces")
	if err := os.MkdirAll(surfaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	surface = filepath.Join(surfaceRoot, "review-fac-626-"+wantSHA[:12])
	rel, err := filepath.Rel(surfaceRoot, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, surface); err != nil {
		t.Fatal(err)
	}
	return surface, wantSHA, actualHead, target
}

// This is the PRODUCTION function runPoolReview calls at both checkpoints
// (before the --no-launch "ready" print and before herdr.Send). Driving it
// directly against a manufactured stale alias is what proves the check
// itself is non-vacuous; the wiring test below proves it is actually called
// from both places.
func TestVerifySurfaceCandidateRefusesAStaleAlias(t *testing.T) {
	surface, wantSHA, actualHead, target := staleSurfaceFixture(t)

	err := verifySurfaceCandidate(surface, wantSHA)
	if err == nil {
		t.Fatal("a surface whose resolved HEAD does not match the requested candidate was accepted; " +
			"a reviewer dispatched here would review the wrong commit and its verdict would be misattributed")
	}
	for _, want := range []string{wantSHA, actualHead, target} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q (requested sha / actual HEAD / resolved target), got: %v", want, err)
		}
	}
}

// Agreement is not refused: a surface whose resolved HEAD IS the requested
// candidate must pass silently.
func TestVerifySurfaceCandidateAcceptsAMatchingSurface(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "candidate commit")
	sha := gitIn(t, repo, "rev-parse", "HEAD")

	surfaceRoot := filepath.Join(base, "review-surfaces")
	os.MkdirAll(surfaceRoot, 0o755)
	surface := filepath.Join(surfaceRoot, "review-fac-626-"+sha[:12])
	rel, err := filepath.Rel(surfaceRoot, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, surface); err != nil {
		t.Fatal(err)
	}

	if err := verifySurfaceCandidate(surface, sha); err != nil {
		t.Fatalf("a surface whose HEAD matches the requested candidate was refused: %v", err)
	}
}

// FAC-626 wiring: the verify call must run BEFORE the --no-launch "ready"
// print AND before herdr.Send delivers the packet -- "not only in the
// --no-launch reporting path" is explicit in the card. Source-scan, in the
// style already established in this package (see
// TestReconciliationIsCalledFromTheProductionIngestPath /
// TestReviewerReadinessIsProvedBeforeLease): reads the shipped source and
// fails if either call site or its ordering is removed.
func TestVerifySurfaceCandidateGuardsBothReadyAndLaunch(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runPoolReview(")
	if !ok {
		t.Fatal("cannot locate runPoolReview")
	}

	verifyCall := "verifySurfaceCandidate(surface, sha)"
	readyPrint := `fmt.Printf("review surface ready`
	sendCall := "herdr.Send(agentName,"

	firstVerify := strings.Index(body, verifyCall)
	if firstVerify < 0 {
		t.Fatal("runPoolReview never calls verifySurfaceCandidate; a stale pooled surface can be reported ready unchecked")
	}
	readyIdx := strings.Index(body, readyPrint)
	if readyIdx < 0 {
		t.Fatal("could not locate the --no-launch ready print to order against")
	}
	if firstVerify > readyIdx {
		t.Fatal("verifySurfaceCandidate runs AFTER the --no-launch ready print; " +
			"an operator or automation reading that line would already have trusted an unverified surface")
	}

	sendIdx := strings.Index(body, sendCall)
	if sendIdx < 0 {
		t.Fatal("could not locate the reviewer packet delivery (herdr.Send) to order against")
	}
	secondVerify := strings.Index(body[readyIdx:], verifyCall)
	if secondVerify < 0 {
		t.Fatal("verifySurfaceCandidate is called only once, covering the --no-launch report but not the launch path -- " +
			"the card explicitly requires checking \"not only in the --no-launch reporting path\"")
	}
	if readyIdx+secondVerify > sendIdx {
		t.Fatal("the launch-path verifySurfaceCandidate call runs AFTER herdr.Send; a reviewer would already have " +
			"been handed the packet before the surface was checked")
	}
}

// FAC-626 root cause: --no-launch's own doc comment says "the lease remains
// held for the review supervisor to release after verdict ingest", but the
// code released it anyway on the --no-launch return path -- freeing the slot
// for an unrelated, later `herd review --pool` call to lease and reset before
// anyone had dispatched a reviewer into the surface this call had just
// reported ready. Source-scan: releaseOnFailure must be set false before the
// --no-launch return, exactly as it already is before the launch path's
// success return.
func TestNoLaunchDoesNotReleaseItsLease(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runPoolReview(")
	if !ok {
		t.Fatal("cannot locate runPoolReview")
	}
	noLaunchIdx := strings.Index(body, "if *noLaunch {")
	if noLaunchIdx < 0 {
		t.Fatal("cannot locate the --no-launch branch")
	}
	returnIdx := strings.Index(body[noLaunchIdx:], "return nil")
	if returnIdx < 0 {
		t.Fatal("cannot locate the --no-launch branch's return")
	}
	branch := body[noLaunchIdx : noLaunchIdx+returnIdx]
	if !strings.Contains(branch, "releaseOnFailure = false") {
		t.Fatal("the --no-launch branch does not set releaseOnFailure = false before returning; " +
			"the lease is released on the way out, freeing the slot for a LATER, unrelated review to " +
			"lease and reset before this surface has been dispatched into -- the FAC-626 live incident")
	}
}
