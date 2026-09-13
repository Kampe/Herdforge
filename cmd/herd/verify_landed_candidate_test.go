package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

// gitRepoWithLandedHead builds a repo whose branch HEAD is already contained in
// origin/main -- the shape of work rebase-merged outside the fleet.
func gitRepoWithLandedHead(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := exec.Command("sh", "-c", "echo one > "+filepath.Join(dir, "f")).Run(); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "one")
	// A REAL origin, not a hand-written tracking ref. The resolver now refuses
	// when the current origin cannot be fetched, because a stale tracking ref
	// cannot answer whether HEAD is already landed -- so the fixture has to
	// offer something to fetch (review b70054a6).
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init origin: %v\n%s", err, out)
	}
	run("remote", "add", "origin", origin)
	run("push", "-q", "origin", "main")
	return dir
}

// TestCandidateResolutionRefusesLandedHead is the FAC-566 regression.
//
// The resolver fell back to worktree HEAD, which is correct only while work is
// unmerged. For work already rebase-merged, HEAD is the LANDED head, so the
// recorded candidate became the merge commit -- and Route B then refused the
// legitimate verdict it existed to authorize.
func TestCandidateResolutionRefusesLandedHead(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	_, err := resolveVerifyLandedCandidate(context.Background(), dir, "reconstruct/cha-2183", verifyLandedBinding{})
	if err == nil {
		t.Fatal("a HEAD already contained in origin/main must not be used as the candidate")
	}
	for _, want := range []string{"already contained", "--candidate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must explain and name the remedy (%q), got %v", want, err)
		}
	}
}

// An explicit candidate always wins: the operator named the object.
func TestExplicitCandidateWins(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	want := "fc3bd108309667d67493a09efc9725f47b15452f"
	got, err := resolveVerifyLandedCandidate(context.Background(), dir, "b", verifyLandedBinding{Candidate: want})
	if err != nil || got != want {
		t.Fatalf("explicit candidate must win, got %q %v", got, err)
	}
}

// An unmerged HEAD is still a valid candidate; this guard must not break the
// ordinary pre-merge harvest.
func TestUnmergedHeadStillResolves(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Advance HEAD past origin/main so it is genuinely unmerged.
	if err := exec.Command("sh", "-c", "echo two >> "+filepath.Join(dir, "f")).Run(); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "two")

	got, err := resolveVerifyLandedCandidate(context.Background(), dir, "b", verifyLandedBinding{})
	if err != nil || got == "" {
		t.Fatalf("an unmerged HEAD must still resolve as the candidate: %q %v", got, err)
	}
}

// The guard the refusal exists for: with origin unreachable, the resolver must
// not answer from a stale tracking ref. Answering "not contained" there is
// exactly how a landed commit gets bound as the reviewed candidate.
func TestCandidateResolutionRefusesWhenOriginCannotBeFetched(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// The tracking ref survives; the remote does not.
	run("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	_, err := resolveVerifyLandedCandidate(context.Background(), dir, "b", verifyLandedBinding{})
	if err == nil {
		t.Fatal("an unreachable origin answered the landed-head guard from a stale tracking ref")
	}
	for _, want := range []string{"could not be fetched", "--candidate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name the cause and the remedy (%q): %v", want, err)
		}
	}
}

// Candidate identity is part of the proof, so resolving it SPENDS THE CALLER'S
// ALLOWANCE (review b70054a6). Before the repair every command here ran on bare
// exec.Command: no deadline, no command or output cap, no owned process group,
// and nothing charged to the budget the route later enforced.
//
// One command covers the HEAD read and nothing after it, so the containment
// guard must stop with the sentinel intact. A resolution that carried an
// allowance of its own -- a fresh context, or a per-helper budget -- would
// simply finish, which is exactly what this refuses to let happen.
func TestCandidateResolutionSpendsTheCallersAllowance(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	gate := &mergeadmit.Gate{RepoDir: dir, ProofBudget: mergeadmit.ProofBudget{MaxCommands: 1}}
	ctx, cancel := gate.ProofContext()
	defer cancel()

	_, err := resolveVerifyLandedCandidate(ctx, dir, "b", verifyLandedBinding{})
	if err == nil {
		t.Fatal("candidate resolution completed on an allowance of one command; it did not spend the caller's budget")
	}
	if !errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands through errors.Is", err)
	}
}

// The HEAD read itself rides the caller's context, so a cancelled run stops
// THERE rather than continuing into the containment guard on a context of its
// own. The stage is named in the refusal, which is what makes "it stopped where
// the caller stopped" checkable rather than merely "something failed".
func TestCandidateResolutionStopsOnACancelledRun(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolveVerifyLandedCandidate(ctx, dir, "b", verifyLandedBinding{})
	if err == nil {
		t.Fatal("a cancelled run resolved a candidate")
	}
	if !strings.Contains(err.Error(), "resolve candidate tip on") {
		t.Fatalf("err = %v, want the candidate tip read to stop on a cancelled run", err)
	}
}
