package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/transfer"
)

// bundleManifestFixture is the CLI-level hermetic fixture: a temp git repo
// with an owned .herd bundle directory holding one real verified bundle.
func bundleManifestFixture(t *testing.T) (repoRoot, bundleDir, bundleName string) {
	t.Helper()
	repoRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoRoot, ".gitignore"), []byte(".herd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "seed.txt"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "seed")
	git("update-ref", "refs/remotes/origin/main", "main")
	bundleDir = filepath.Join(repoRoot, ".herd", "coordinator-resume", "completion-recovery-test")
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		t.Fatal(err)
	}
	bundleName = "cli-transfer.bundle"
	cmd := exec.Command("git", "-C", repoRoot, "bundle", "create", filepath.Join(bundleDir, bundleName), "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	return repoRoot, bundleDir, bundleName
}

// TestBundleManifestCLIDryRunAndWrite runs the native producer end to end:
// dry run describes the exact v2 manifest without creating anything, then
// --write atomically creates it and the reclaim consumer accepts it.
func TestBundleManifestCLIDryRunAndWrite(t *testing.T) {
	repoRoot, bundleDir, bundleName := bundleManifestFixture(t)
	t.Setenv("HERD_CANONICAL_ROOT", repoRoot)
	out := filepath.Join(bundleDir, "retention-manifest.json")

	dryRun := exec.Command(buildHerd(t), "bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "forge-orchestrator-test")
	dryOut, err := dryRun.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, dryOut)
	}
	if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
		t.Fatalf("dry run must not create the manifest: %v", statErr)
	}

	write := exec.Command(buildHerd(t), "bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "forge-orchestrator-test", "--out", out, "--write")
	writeOut, err := write.CombinedOutput()
	if err != nil {
		t.Fatalf("write: %v\n%s", err, writeOut)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\"version\": 2") || !strings.Contains(string(body), "\"authority\": \"forge-orchestrator-test\"") ||
		!strings.Contains(string(body), "\"mod_time_unix_nano\": ") || strings.Contains(string(body), "e+") {
		t.Fatalf("manifest schema/identity mismatch:\n%s", body)
	}
	// Reclaim consumer parity: the loader admits the produced manifest.
	if _, err := transfer.LoadRetentionManifest(bundleDir, out); err != nil {
		t.Fatalf("reclaim loader rejected the produced manifest: %v", err)
	}
}

// TestBundleManifestCLIRefusals covers the CLI refusal surface: implicit
// selection, unknown flags (FAC-806-style word rejection), missing
// authority, and overwrite refusal.
func TestBundleManifestCLIRefusals(t *testing.T) {
	repoRoot, bundleDir, bundleName := bundleManifestFixture(t)
	t.Setenv("HERD_CANONICAL_ROOT", repoRoot)
	cases := [][]string{
		{"bundle-manifest", "--root", bundleDir, "--authority", "a"},
		{"bundle-manifest", "--root", bundleDir},
		{"bundle-manifest", "--root", bundleDir, "--bundle", bundleName},
		{"bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "a", "status"},
		{"bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "a", "--out", filepath.Join(bundleDir, "m.json"), "--write", "--dry-run"},
	}
	for _, tc := range cases {
		cmd := exec.Command(buildHerd(t), tc...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("herd %v: exit 0, want nonzero\n%s", tc, out)
		}
	}
	// Write once, then refuse overwrite.
	out := filepath.Join(bundleDir, "retention-manifest.json")
	cmd := exec.Command(buildHerd(t), "bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "a", "--out", out, "--write")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("first write: %v\n%s", err, outBytes)
	}
	cmd = exec.Command(buildHerd(t), "bundle-manifest", "--root", bundleDir, "--bundle", bundleName, "--authority", "a", "--out", out, "--write")
	outBytes, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(outBytes), "never overwritten") {
		t.Fatalf("overwrite must be refused: %v\n%s", err, outBytes)
	}
}

// TestBundleManifestCLILinkedWorktreeOwnedState reproduces the PR811
// production integration regression hermetically: a REAL registered linked
// worktree's own .herd bundle directory is owned state and must be
// acceptable from the canonical repo cwd with a RELATIVE --root, while a
// sibling foreign repository and symlink escapes stay refused.
func TestBundleManifestCLILinkedWorktreeOwnedState(t *testing.T) {
	repoRoot, bundleDir, bundleName := bundleManifestFixture(t)
	data, err := os.ReadFile(filepath.Join(bundleDir, bundleName))
	if err != nil {
		t.Fatal(err)
	}
	// Register a real linked worktree and give it its own owned state.
	wt := filepath.Join(repoRoot, "sibling-wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", wt, "-b", "wt-owned-state").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	wtHerd := filepath.Join(wt, ".herd", "coordinator-resume", "linked-transport")
	if err := os.MkdirAll(wtHerd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtHerd, "wt-transfer.bundle"), data, 0644); err != nil {
		t.Fatal(err)
	}
	// A sibling FOREIGN repository with its own .herd bundle directory.
	foreignRoot := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", foreignRoot).CombinedOutput(); err != nil {
		t.Fatalf("foreign init: %v\n%s", err, out)
	}
	foreignHerd := filepath.Join(foreignRoot, ".herd", "coordinator-resume", "foreign-transport")
	if err := os.MkdirAll(foreignHerd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignHerd, "foreign.bundle"), data, 0644); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, cwd string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(buildHerd(t), args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "HERD_CANONICAL_ROOT="+repoRoot)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("relative root into a registered linked worktree is accepted", func(t *testing.T) {
		out, err := run(t, repoRoot,
			"bundle-manifest",
			"--root", filepath.Join("sibling-wt", ".herd", "coordinator-resume", "linked-transport"),
			"--bundle", "wt-transfer.bundle",
			"--authority", "linked-worktree-owned-state",
			"--out", filepath.Join("sibling-wt", ".herd", "coordinator-resume", "linked-transport", "retention-linked.json"),
			"--dry-run",
		)
		if err != nil {
			t.Fatalf("linked-worktree owned .herd state must be accepted: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"version": 2`) || !strings.Contains(out, "wt-transfer.bundle") {
			t.Fatalf("dry run must describe the exact intended manifest:\n%s", out)
		}
		if _, statErr := os.Lstat(filepath.Join(wtHerd, "retention-linked.json")); statErr == nil {
			t.Fatal("dry run must not create the manifest")
		}
	})

	t.Run("sibling foreign repository is refused", func(t *testing.T) {
		out, err := run(t, repoRoot,
			"bundle-manifest",
			"--root", foreignHerd,
			"--bundle", "foreign.bundle",
			"--authority", "foreign",
			"--dry-run",
		)
		if err == nil || !strings.Contains(out, "another repository") {
			t.Fatalf("foreign repository must be refused: %v\n%s", err, out)
		}
	})

	t.Run("symlinked leaf is refused", func(t *testing.T) {
		escape := filepath.Join(wt, ".herd", "coordinator-resume", "escape-leaf")
		if err := os.Symlink(foreignHerd, escape); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		out, err := run(t, repoRoot,
			"bundle-manifest",
			"--root", escape,
			"--bundle", "foreign.bundle",
			"--authority", "escape",
			"--dry-run",
		)
		if err == nil || !strings.Contains(out, "real directory") {
			t.Fatalf("symlinked root must be refused: %v\n%s", err, out)
		}
	})

	t.Run("symlinked parent escape is refused", func(t *testing.T) {
		escape := filepath.Join(wt, ".herd", "coordinator-resume", "escape-parent")
		if err := os.Symlink(foreignRoot, escape); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		out, err := run(t, repoRoot,
			"bundle-manifest",
			"--root", filepath.Join(escape, ".herd", "coordinator-resume", "foreign-transport"),
			"--bundle", "foreign.bundle",
			"--authority", "escape",
			"--dry-run",
		)
		if err == nil || !strings.Contains(out, "another repository") {
			t.Fatalf("parent-symlink escape must be refused: %v\n%s", err, out)
		}
	})
}
