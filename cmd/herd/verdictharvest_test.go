package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The harvester must never overwrite a local artifact. A local copy may already
// be ingested, and clobbering it would resurrect a settled verdict -- the same
// class of defect as the bulk inbox replay that once reverted 443 cards.
func TestVerdictHarvest_NeverOverwritesLocalArtifact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	inbox := filepath.Join(dir, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(inbox, "v.md")
	if err := os.WriteFile(local, []byte("LOCAL COPY - already ingested\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HERD_PROJECT_ROOT", dir)
	t.Setenv("HERD_ROOT", filepath.Join(dir, "lane"))
	os.Args = []string{"herd", "verdict-harvest", "--dry-run"}
	// No remote configured: this must FAIL rather than report "nothing waiting".
	// An unreachable remote is not evidence that no verdicts exist.
	if err := runVerdictHarvest(); err == nil {
		t.Fatal("an unreachable remote must be an error, not a quiet empty harvest")
	}

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "LOCAL COPY - already ingested\n" {
		t.Fatalf("local artifact was modified: %q", got)
	}
}

// This drives the shipped command from a non-project cwd containing a filename
// collision. The remote artifact must land only in the explicit project root:
// choosing HERD_ROOT makes the fetch fail, while a cwd-relative stat silently
// skips the artifact it would otherwise extract into the project.
func TestVerdictHarvestUsesCanonicalProjectRootForFetchStatAndWrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	binary := buildHerd(t)

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	remote := t.TempDir()
	git(remote, "init", "-q", "--bare")
	project := t.TempDir()
	git(project, "init", "-q", "-b", "main")
	git(project, "commit", "-q", "--allow-empty", "-m", "base")
	git(project, "remote", "add", "origin", remote)
	git(project, "push", "-q", "origin", "main")

	publisher := t.TempDir()
	git(publisher, "init", "-q", "-b", "main")
	git(publisher, "remote", "add", "origin", remote)
	remoteArtifact := filepath.Join(publisher, ".herd", "review", "inbox", "verdict.md")
	if err := os.MkdirAll(filepath.Dir(remoteArtifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteArtifact, []byte("REMOTE VERDICT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(publisher, "add", ".herd/review/inbox/verdict.md")
	git(publisher, "commit", "-q", "-m", "publish verdict")
	git(publisher, "push", "-q", "origin", "HEAD:refs/heads/verdicts/reviewer")

	cwd := t.TempDir()
	cwdArtifact := filepath.Join(cwd, ".herd", "review", "inbox", "verdict.md")
	if err := os.MkdirAll(filepath.Dir(cwdArtifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwdArtifact, []byte("CWD COLLISION\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lane := t.TempDir()
	t.Setenv("HERD_PROJECT_ROOT", project)
	t.Setenv("HERD_ROOT", lane)

	cmd := exec.Command(binary, "verdict-harvest")
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verdict-harvest: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(project, ".herd", "review", "inbox", "verdict.md"))
	if err != nil {
		t.Fatalf("read harvested project artifact: %v\ncommand output: %s", err, out)
	}
	if string(got) != "REMOTE VERDICT\n" {
		t.Fatalf("harvested project artifact = %q, want remote verdict", got)
	}
	stillLocal, err := os.ReadFile(cwdArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(stillLocal) != "CWD COLLISION\n" {
		t.Fatalf("cwd collision was modified: %q", stillLocal)
	}
}
