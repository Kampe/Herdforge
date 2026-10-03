package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
)

func gitInitIssuanceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-q", "-m", "base")
	return dir
}

func gitSHA(t *testing.T, dir string, rev string) string {
	t.Helper()
	out, err := gitC(dir, "rev-parse", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return out
}

func gitIssuanceRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestVerifierIssuanceBaseUsesMergeBaseWhenMainAdvanced(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	base := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "checkout", "-q", "-b", "feature")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "checkout", "-q", "main")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "later-main")
	originMain := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "update-ref", "refs/remotes/origin/main", originMain)
	if gitIsAncestor(dir, originMain, candidate) {
		t.Fatal("fixture did not advance origin/main past the candidate")
	}
	got, err := verifierIssuanceBase(dir, originMain, candidate, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("base=%s want merge-base %s (not advanced main %s)", got, base, originMain)
	}
	if !gitIsAncestor(dir, got, candidate) {
		t.Fatal("selected base is not an ancestor of the candidate")
	}
}

func TestVerifierIssuanceBaseRefusesPlaceholderABC(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	cand := gitSHA(t, dir, "HEAD")
	if _, err := verifierIssuanceBase(dir, cand, cand, "abc", ""); err == nil {
		t.Fatal("placeholder abc was accepted as verifier base")
	}
}

func TestVerifierIssuanceBaseRefusesExplicitNonAncestor(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	base := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "later-main")
	later := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "update-ref", "HEAD", base)
	candidate := gitSHA(t, dir, "HEAD")
	if _, err := verifierIssuanceBase(dir, later, candidate, "", later); err == nil {
		t.Fatal("explicit advanced main was accepted as verifier base")
	}
}

func TestVerifierIssuanceBasePreservesAuthenticatedAncestralBase(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	fork := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "checkout", "-q", "-b", "feature")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "builder-base")
	auth := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "checkout", "-q", "main")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "later-main")
	originMain := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "update-ref", "refs/remotes/origin/main", originMain)
	if auth == fork {
		t.Fatal("authenticated base must be a strict descendant of the merge-base")
	}
	mb, err := gitC(dir, "merge-base", candidate, originMain)
	if err != nil {
		t.Fatal(err)
	}
	if mb != fork {
		t.Fatalf("merge-base=%s want fork %s", mb, fork)
	}
	got, err := verifierIssuanceBase(dir, originMain, candidate, auth, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != auth {
		t.Fatalf("authenticated base: got %s want %s", got, auth)
	}
	if got == mb {
		t.Fatal("authenticated preference collapsed onto merge-base; fixture is vacuous")
	}
}

func TestVerifierIssuanceBaseRefusesExplicitConflictWithAuthenticated(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	fork := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "checkout", "-q", "-b", "feature")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "builder-base")
	auth := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	if _, err := verifierIssuanceBase(dir, fork, candidate, auth, fork); err == nil {
		t.Fatal("explicit --base that conflicts with authenticated builder base was accepted")
	}
}

func TestVerifierIssuanceBaseAllowsExplicitMatchingAuthenticated(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	gitIssuanceRun(t, dir, "checkout", "-q", "-b", "feature")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "builder-base")
	auth := gitSHA(t, dir, "HEAD")
	gitIssuanceRun(t, dir, "commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	got, err := verifierIssuanceBase(dir, auth, candidate, auth, auth)
	if err != nil || got != auth {
		t.Fatalf("matching explicit --base: got %s err=%v want %s", got, err, auth)
	}
}

func TestAuthenticatedBuilderBaseAbsentIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := authenticatedBuilderBase(dir, dir, "abc123abc123")
	if err != nil || got != "" {
		t.Fatalf("absent context: got %q err=%v", got, err)
	}
}

func TestAuthenticatedBuilderBasePresentInvalidFailsClosed(t *testing.T) {
	repo, keyDir := t.TempDir(), t.TempDir()
	attestKeyDir(t, keyDir)
	cmd := exec.Command("git", "-C", repo, "init", "-q", "-b", "main")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	signer := fixtureSigner(t, keyDir, repo)
	target := filepath.Join(repo, "wt")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	base := strings.Repeat("c", 40)

	issue := func(role, cand string) dispatch.TaskContext {
		t.Helper()
		tc := dispatch.TaskContext{
			ProviderType: "kaneo", ProjectID: "proj-x", Repository: dispatch.RepositoryIdentityOrName(repo, "herdforge-test"),
			Role: role, TaskRef: "FAC-1", TaskID: "t1", Branch: "herd/fac-1",
			BaseSHA: base, CandidateSHA: cand, LeaseID: "lease-1", LeaseGeneration: 1,
			LeaseTaskRef: "FAC-1", SessionID: "sess-1", AllowedOps: dispatch.OpsForRole(role),
			ExpiresAt: time.Now().Add(time.Hour),
		}
		signed, err := signer.Issue(tc)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}

	t.Run("valid-worker", func(t *testing.T) {
		signed := issue(dispatch.RoleWorker, candidate)
		if err := dispatch.WriteTaskContext(target, signed); err != nil {
			t.Fatal(err)
		}
		got, err := authenticatedBuilderBase(repo, target, candidate)
		if err != nil || got != base {
			t.Fatalf("valid worker: got %q err=%v want %s", got, err, base)
		}
	})

	t.Run("tampered-signature", func(t *testing.T) {
		signed := issue(dispatch.RoleWorker, candidate)
		signed.Signature = strings.Repeat("0", len(signed.Signature))
		if err := dispatch.WriteTaskContext(target, signed); err != nil {
			t.Fatal(err)
		}
		got, err := authenticatedBuilderBase(repo, target, candidate)
		if err == nil || got != "" {
			t.Fatalf("tampered signature must fail closed: got %q err=%v", got, err)
		}
		if !strings.Contains(err.Error(), "failed authentication") {
			t.Fatalf("want authentication diagnostic, got %v", err)
		}
	})

	t.Run("candidate-mismatch", func(t *testing.T) {
		signed := issue(dispatch.RoleWorker, other)
		if err := dispatch.WriteTaskContext(target, signed); err != nil {
			t.Fatal(err)
		}
		got, err := authenticatedBuilderBase(repo, target, candidate)
		if err == nil || got != "" {
			t.Fatalf("candidate mismatch must fail closed: got %q err=%v", got, err)
		}
		if !strings.Contains(err.Error(), "not "+candidate) {
			t.Fatalf("want candidate mismatch diagnostic, got %v", err)
		}
	})

	t.Run("wrong-role", func(t *testing.T) {
		signed := issue(dispatch.RoleVerifier, candidate)
		if err := dispatch.WriteTaskContext(target, signed); err != nil {
			t.Fatal(err)
		}
		got, err := authenticatedBuilderBase(repo, target, candidate)
		if err == nil || got != "" {
			t.Fatalf("wrong role must fail closed: got %q err=%v", got, err)
		}
		if !strings.Contains(err.Error(), "not worker or recovery") {
			t.Fatalf("want role diagnostic, got %v", err)
		}
	})

	t.Run("missing-verifier-key", func(t *testing.T) {
		signed := issue(dispatch.RoleWorker, candidate)
		if err := dispatch.WriteTaskContext(target, signed); err != nil {
			t.Fatal(err)
		}
		got, err := authenticatedBuilderBase(t.TempDir(), target, candidate)
		if err == nil || got != "" {
			t.Fatalf("missing key must fail closed: got %q err=%v", got, err)
		}
		if !strings.Contains(err.Error(), "cannot authenticate") {
			t.Fatalf("want key diagnostic, got %v", err)
		}
	})
}
