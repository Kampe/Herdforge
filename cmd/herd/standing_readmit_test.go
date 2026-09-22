package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

func standingGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func standingCommit(t *testing.T, dir, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "note"), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	standingGit(t, dir, "add", "note")
	standingGit(t, dir, "commit", "-q", "-m", msg)
	return standingGit(t, dir, "rev-parse", "HEAD")
}

// standingLaneRepo builds an origin plus an existing standing clone under wt/<lane>.
func standingLaneRepo(t *testing.T) (root, origin, laneDir string, base string) {
	t.Helper()
	root = t.TempDir()
	origin = filepath.Join(root, "origin")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	standingGit(t, origin, "init", "-q", "-b", "main")
	standingGit(t, origin, "config", "user.email", "t@t")
	standingGit(t, origin, "config", "user.name", "t")
	base = standingCommit(t, origin, "base")
	laneDir = filepath.Join(root, "wt", "scout")
	if err := os.MkdirAll(filepath.Dir(laneDir), 0o755); err != nil {
		t.Fatal(err)
	}
	standingGit(t, root, "clone", "-q", origin, laneDir)
	t.Chdir(root)
	return root, origin, laneDir, base
}

func TestPrepareStandingWorktreeRefreshesBehindCleanLane(t *testing.T) {
	_, origin, laneDir, _ := standingLaneRepo(t)
	fresh := standingCommit(t, origin, "origin-ahead")
	lane := &config.LaneDef{Name: "scout", Worktree: "wt/scout"}
	if err := prepareStandingWorktree(lane); err != nil {
		t.Fatalf("behind clean lane must refresh: %v", err)
	}
	got := standingGit(t, laneDir, "rev-parse", "HEAD")
	if got != fresh {
		t.Fatalf("stale standing raise admitted HEAD %s, origin/main is %s", got, fresh)
	}
}

func TestPrepareStandingWorktreeRefusesDirtyLane(t *testing.T) {
	_, origin, laneDir, _ := standingLaneRepo(t)
	standingCommit(t, origin, "origin-ahead")
	if err := os.WriteFile(filepath.Join(laneDir, "dirty"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lane := &config.LaneDef{Name: "scout", Worktree: "wt/scout"}
	err := prepareStandingWorktree(lane)
	if err == nil || !strings.Contains(err.Error(), "dirty") || !strings.Contains(err.Error(), "ahead") {
		t.Fatalf("dirty standing raise must refuse with distance, got %v", err)
	}
}

func TestPrepareStandingWorktreeRefusesUnmergedAhead(t *testing.T) {
	_, _, laneDir, _ := standingLaneRepo(t)
	standingCommit(t, laneDir, "lane-ahead")
	lane := &config.LaneDef{Name: "scout", Worktree: "wt/scout"}
	err := prepareStandingWorktree(lane)
	if err == nil || !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), "ahead") {
		t.Fatalf("ahead standing raise must refuse with distance, got %v", err)
	}
}

func TestAdmitStandingWorktreeFetchHonorsProofDeadline(t *testing.T) {
	_, _, laneDir, _ := standingLaneRepo(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, accErr := ln.Accept()
		if accErr != nil {
			return
		}
		defer c.Close()
		time.Sleep(30 * time.Second)
	}()
	standingGit(t, laneDir, "remote", "set-url", "origin", "git://"+ln.Addr().String()+"/")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	prev := cliProofBudget
	cliProofBudget = mergeadmit.ProofBudget{Deadline: 400 * time.Millisecond}
	t.Cleanup(func() { cliProofBudget = prev })
	start := time.Now()
	err = prepareStandingWorktree(&config.LaneDef{Name: "scout", Worktree: "wt/scout"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled fetch must refuse")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fetch hung %s beyond proof deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("want bounded fetch timeout, got %v after %s", err, elapsed)
	}
}

func TestPrepareStandingWorktreeFreshZeroZeroStillRaises(t *testing.T) {
	_, _, laneDir, base := standingLaneRepo(t)
	lane := &config.LaneDef{Name: "scout", Worktree: "wt/scout"}
	if err := prepareStandingWorktree(lane); err != nil {
		t.Fatalf("0/0 standing raise must proceed: %v", err)
	}
	got := standingGit(t, laneDir, "rev-parse", "HEAD")
	if got != base {
		t.Fatalf("healthy raise moved HEAD %s want %s", got, base)
	}
}
