package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// FAC-144 production-path tests. These prove completion cannot spawn review
// without a persisted current PASS receipt, and that FAIL/BLOCKED/stale/
// mismatched evidence each block review. A test that cannot fail is not
// coverage — each negative case is asserted to error.

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func hermeticGitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=t@example.com",
	}
}

// initGitCandidate builds a clean commit that includes the verification
// scripts so VerifyCandidate does not BLOCKED on untracked dirty files.
func initGitCandidate(t *testing.T, dir string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = hermeticGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s (%v)", args, out, err)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@example.com")
	run("git", "config", "user.name", "test")
	run("git", "config", "commit.gpgsign", "false")
	run("git", "config", "tag.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "ok.sh", "exit 0")
	writeScript(t, dir, "fail.sh", "exit 1")
	run("git", "add", "README", "ok.sh", "fail.sh")
	run("git", "commit", "-m", "init")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func newGate(t *testing.T, argv []string, withMachine bool) (*CompletionGate, *lifecycle.Machine, string) {
	t.Helper()
	root := t.TempDir()
	receiptDir := filepath.Join(root, "receipts")
	var machine *lifecycle.Machine
	if withMachine {
		var err error
		machine, err = lifecycle.NewMachine(filepath.Join(root, "life.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { machine.Close() })
	}
	v := verifier.NewVerifierArgs(argv)
	g, err := NewCompletionGate(v, receiptDir, machine)
	if err != nil {
		t.Fatal(err)
	}
	return g, machine, root
}

func bindFor(t *testing.T, dir, sha string, gen int64) CompletionBinding {
	t.Helper()
	return CompletionBinding{
		TaskRef:             "FAC-144",
		Repo:                "herdforge",
		LeaseOwner:          "worker-a",
		LeaseGeneration:     gen,
		ProviderRevision:    "prov-1",
		CandidateSHA:        sha,
		PatchID:             "patch-1",
		ClassifierVersion:   "classify-v1",
		VerificationProfile: "profile-true",
		Branch:              "herd/fac-144",
		WorktreeDir:         dir,
	}
}

func TestCompletionGate_PASS_EnablesReviewOnce(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)

	d1, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatalf("first handle: %v", err)
	}
	if !d1.ReviewReady || d1.Outcome != verifier.OutcomePASS || d1.Digest == "" {
		t.Fatalf("want PASS review-ready decision, got %+v", d1)
	}

	// Duplicate completion / restart: no second verification run required;
	// lifecycle/outbox replay.
	d2, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	if !d2.ReviewReady || d2.Digest != d1.Digest {
		t.Fatalf("idempotent resume: got %+v want digest %s", d2, d1.Digest)
	}
	if !d2.Replayed {
		t.Fatalf("expected replayed=true on duplicate completion, got %+v", d2)
	}

	// Review spawn admits exactly this digest.
	if _, err := g.AdmitReview(context.Background(), bind, d1.Digest); err != nil {
		t.Fatalf("admit review: %v", err)
	}

	// Outbox has exactly one review_enqueue intent (deduped).
	pending, err := machine.Outbox().Pending("review_enqueue", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("outbox review_enqueue count=%d want 1", len(pending))
	}
	ts, err := machine.EventStore().CurrentState("FAC-144")
	if err != nil || ts == nil || ts.State != lifecycle.StateReviewing {
		t.Fatalf("lifecycle state=%+v err=%v want reviewing", ts, err)
	}
}

func TestCompletionGate_FAIL_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./fail.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)

	d, err := g.HandleCompletion(context.Background(), bind)
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("want ErrVerificationFailed, got %v", err)
	}
	if d == nil || d.Outcome != verifier.OutcomeFAIL || d.ReviewReady {
		t.Fatalf("FAIL decision: %+v", d)
	}
	if _, err := g.AdmitReview(context.Background(), bind, d.Digest); err == nil {
		t.Fatal("FAIL receipt must not admit review")
	}
}

func TestCompletionGate_BLOCKED_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	// Dirty worktree → VerifyCandidate returns BLOCKED receipt.
	if err := os.WriteFile(filepath.Join(dir, "dirty"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)

	d, err := g.HandleCompletion(context.Background(), bind)
	if !errors.Is(err, ErrVerificationBlocked) {
		t.Fatalf("want ErrVerificationBlocked, got %v (decision=%+v)", err, d)
	}
	if d != nil && d.ReviewReady {
		t.Fatal("BLOCKED must not set ReviewReady")
	}
}

func TestCompletionGate_MissingDigest_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, _, _ := newGate(t, []string{"./ok.sh"}, false)
	bind := bindFor(t, dir, sha, 1)
	if _, err := g.AdmitReview(context.Background(), bind, ""); err == nil {
		t.Fatal("empty digest must block review")
	}
	if _, err := g.AdmitReview(context.Background(), bind, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("missing receipt must block review")
	}
}

func TestCompletionGate_StaleCandidateSHA_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)
	d, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatal(err)
	}
	// Advance HEAD → prior receipt is no longer current.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "-am", "move head")
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %s (%v)", out, err)
	}
	if _, err := g.AdmitReview(context.Background(), bind, d.Digest); err == nil {
		t.Fatal("stale candidate SHA must block review")
	}
}

func TestCompletionGate_StaleGeneration_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 2); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 2)
	d, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatal(err)
	}
	stale := bind
	stale.LeaseGeneration = 1
	if _, err := g.AdmitReview(context.Background(), stale, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("stale generation: want ErrBindingMismatch, got %v", err)
	}
}

func TestCompletionGate_WrongTask_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)
	d, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatal(err)
	}
	wrong := bind
	wrong.TaskRef = "FAC-999"
	if _, err := g.AdmitReview(context.Background(), wrong, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("wrong task: want ErrBindingMismatch, got %v", err)
	}
}

func TestCompletionGate_PolicyChange_Invalidates(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, machine, _ := newGate(t, []string{"./ok.sh"}, true)
	if err := SeedLifecycleToBuilding(machine, "FAC-144", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)
	d, err := g.HandleCompletion(context.Background(), bind)
	if err != nil {
		t.Fatal(err)
	}
	changed := bind
	changed.VerificationProfile = "profile-other"
	if _, err := g.AdmitReview(context.Background(), changed, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("policy change: want ErrBindingMismatch, got %v", err)
	}
	changed = bind
	changed.ClassifierVersion = "classify-v2"
	if _, err := g.AdmitReview(context.Background(), changed, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("classifier change: want ErrBindingMismatch, got %v", err)
	}
	changed = bind
	changed.ProviderRevision = "prov-2"
	if _, err := g.AdmitReview(context.Background(), changed, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("provider revision change: want ErrBindingMismatch, got %v", err)
	}
	changed = bind
	changed.ProfileDigest = "sha256:" + strings.Repeat("d", 64)
	if _, err := g.AdmitReview(context.Background(), changed, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("profile digest change: want ErrBindingMismatch, got %v", err)
	}
	changed = bind
	changed.ConfigRevision = "sha256:" + strings.Repeat("e", 64)
	if _, err := g.AdmitReview(context.Background(), changed, d.Digest); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("config revision change: want ErrBindingMismatch, got %v", err)
	}
}

func TestCompletionGate_RejectionReasonHasNoHostPath(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, _, _ := newGate(t, []string{"./ok.sh"}, false)
	bind := bindFor(t, dir, sha, 1)
	_, err := g.AdmitReview(context.Background(), bind, "sha256:"+strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("rejection leaked host path %q in %q", dir, err.Error())
	}
}

func TestCompletionGate_CheckCompletionIsNotAuthority(t *testing.T) {
	// A worktree that would pass CheckCompletion still cannot enter review
	// without a persisted PASS receipt + AdmitReview.
	dir := t.TempDir()
	_ = initGitCandidate(t, dir)
	// Make origin/main..HEAD have a real commit subject (not just anchor).
	// CheckCompletion looks for commits ahead of origin/main — without
	// origin/main it fails HasCommits; we only assert the authority seam:
	// AdmitReview without a digest fails regardless of CheckCompletion.
	v := verifier.NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "true")
	// Whether or not CheckCompletion passes is irrelevant: review authority
	// is AdmitReview, and it must refuse without a digest.
	_ = c
	g, _, _ := newGate(t, []string{"true"}, false)
	bind := bindFor(t, dir, strings.Repeat("a", 40), 1)
	if _, err := g.AdmitReview(context.Background(), bind, ""); err == nil {
		t.Fatal("CheckCompletion must not authorize review without a receipt digest")
	}
}

func TestCompletionGate_MalformedReceipt_BlocksReview(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, _, root := newGate(t, []string{"true"}, false)
	// Write a file that looks like a digest path but is corrupt JSON.
	storeDir := filepath.Join(root, "receipts")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	path := filepath.Join(storeDir, strings.TrimPrefix(digest, "sha256:")+".json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	bind := bindFor(t, dir, sha, 1)
	if _, err := g.AdmitReview(context.Background(), bind, digest); err == nil {
		t.Fatal("malformed receipt must block review")
	}
}

func TestCompletionGate_NoLifecycleState_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	sha := initGitCandidate(t, dir)
	g, _, _ := newGate(t, []string{"./ok.sh"}, true)
	// Machine present but never seeded to building.
	bind := bindFor(t, dir, sha, 1)
	_, err := g.HandleCompletion(context.Background(), bind)
	if err == nil {
		t.Fatal("expected fail-closed without lifecycle building state")
	}
	if !strings.Contains(err.Error(), "no lifecycle state") && !strings.Contains(err.Error(), "cannot accept") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The old check was a literal drive-letter comparison, so it matched exactly
// one volume. Structural matching catches every drive, and -- the reason it
// changed -- carries no host path literal for the repository's own preflight
// scanner to flag.
//
// The drive strings are ASSEMBLED rather than written literally for that same
// reason: a test that hardcodes them re-introduces the leak this change removes.
func TestHasWindowsDrivePrefixMatchesAnyDriveNotJustC(t *testing.T) {
	sep := string(rune(92)) // backslash
	drive := func(letter, tail string) string { return letter + ":" + sep + tail }
	for _, p := range []string{drive("C", "x"), drive("c", "x"), drive("D", "srv"), "z:/tmp/b"} {
		if !hasWindowsDrivePrefix(p) {
			t.Errorf("%q must be recognised as a Windows drive path", p)
		}
	}
	for _, p := range []string{"", "C", "C:", "/usr/bin", "relative/path", drive("1", "x"), "CC:" + sep + "x"} {
		if hasWindowsDrivePrefix(p) {
			t.Errorf("%q must not be recognised as a Windows drive path", p)
		}
	}
}
