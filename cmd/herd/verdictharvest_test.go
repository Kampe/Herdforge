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

	t.Setenv("HERD_ROOT", dir)
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
