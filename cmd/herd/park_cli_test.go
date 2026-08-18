package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func TestParkListJSON(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	git := func(args ...string) string {
		t.Helper()
		out, err := testgit.Command(dir, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("config", "commit.gpgSign", "false")
	git("config", "tag.gpgSign", "false")
	git("checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "seed")
	sha := git("rev-parse", "HEAD")

	herd := func(args ...string) (stdout, stderr string, err error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		var outBuf, errBuf strings.Builder
		cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
		err = cmd.Run()
		return outBuf.String(), errBuf.String(), err
	}

	// Push will fail (no origin remote) — that's expected; the park is
	// still created locally and must still show up in `list --json`.
	if _, stderr, err := herd("park", "park", "myslug", sha, "-m", "resume note"); err == nil {
		t.Fatalf("expected park park to fail on push (no origin), got success; stderr: %s", stderr)
	}

	stdout, stderr, err := herd("park", "list", "--json")
	if err != nil {
		t.Fatalf("herd park list --json: %v\nstderr: %s", err, stderr)
	}

	var result struct {
		Commits []struct {
			Tag     string `json:"tag"`
			Commit  string `json:"commit"`
			Message string `json:"message"`
		} `json:"commits"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("herd park list --json produced invalid JSON: %v\noutput: %s", err, stdout)
	}
	if result.Total != 1 || len(result.Commits) != 1 {
		t.Fatalf("expected 1 parked commit, got %+v", result)
	}
	if result.Commits[0].Tag != "parked/myslug" {
		t.Errorf("Tag = %q, want parked/myslug", result.Commits[0].Tag)
	}
	if result.Commits[0].Message != "seed" {
		t.Errorf("Message = %q, want commit subject %q (not the park -m resume note)", result.Commits[0].Message, "seed")
	}
}
