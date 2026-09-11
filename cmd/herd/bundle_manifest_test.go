package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
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

// TestBundleManifestCLILinkedCwdSharedLock runs BOTH native commands from a
// REAL registered linked-worktree cwd with NO HERD_CANONICAL_ROOT override
// and proves one canonical shared-lock identity: while the canonical lock is
// held, a short-wait linked-cwd invocation must be refused by contention;
// after release, the same invocation succeeds. The consumer must accept the
// manifest the producer published from the same linked cwd.
func TestBundleManifestCLILinkedCwdSharedLock(t *testing.T) {
	repoRoot, bundleDir, bundleName := bundleManifestFixture(t)
	data, err := os.ReadFile(filepath.Join(bundleDir, bundleName))
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(repoRoot, "lock-wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", wt, "-b", "wt-shared-lock").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	wtHerd := filepath.Join(wt, ".herd", "coordinator-resume", "shared-lock-transport")
	if err := os.MkdirAll(wtHerd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtHerd, "shared-lock.bundle"), data, 0644); err != nil {
		t.Fatal(err)
	}
	// Run children from the LINKED worktree cwd with the canonical-root
	// override explicitly ABSENT — the default lock must still resolve to
	// the canonical common .git.
	runNoOverride := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(buildHerd(t), args...)
		cmd.Dir = wt
		env := os.Environ()
		filtered := env[:0]
		for _, e := range env {
			if !strings.HasPrefix(e, "HERD_CANONICAL_ROOT=") {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	bundleArgs := func(extra ...string) []string {
		args := []string{"bundle-manifest",
			"--root", filepath.Join(".herd", "coordinator-resume", "shared-lock-transport"),
			"--bundle", "shared-lock.bundle",
			"--authority", "linked-cwd-shared-lock"}
		return append(args, extra...)
	}

	// Publish the manifest from the linked cwd (producer, no override).
	if out, err := runNoOverride(t, bundleArgs("--out", filepath.Join(".herd", "coordinator-resume", "shared-lock-transport", "retention-linked-cwd.json"), "--write", "--lock-wait", "10s")...); err != nil {
		t.Fatalf("linked-cwd producer must resolve the canonical shared lock without an env override: %v\n%s", err, out)
	}
	if _, statErr := os.Lstat(filepath.Join(wtHerd, "retention-linked-cwd.json")); statErr != nil {
		t.Fatalf("manifest must be published inside the linked worktree's owned state: %v", statErr)
	}

	// Hold the CANONICAL shared lock in this test process: a linked-cwd
	// invocation with a short wait must be refused by contention — proving
	// both identities agree on one canonical lock.
	canonicalLock := lock.NewDirLock(filepath.Join(repoRoot, lock.DefaultRelDir))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := canonicalLock.Acquire(ctx, 10*time.Second, "test holds the canonical shared lock"); err != nil {
		t.Fatalf("test could not hold the canonical lock: %v", err)
	}
	out, err := runNoOverride(t, bundleArgs("--dry-run", "--lock-wait", "1s")...)
	if err == nil || !strings.Contains(out, "native lock") {
		t.Fatalf("linked-cwd invocation must contend on the CANONICAL lock: %v\n%s", err, out)
	}
	canonicalLock.Release()

	// After release, the same invocation succeeds.
	if out, err := runNoOverride(t, bundleArgs("--dry-run")...); err != nil {
		t.Fatalf("linked-cwd invocation must succeed after the canonical lock is released: %v\n%s", err, out)
	}

	// Consumer: from the same linked cwd and env, reclaim accepts the
	// manifest the producer published (dry run never deletes).
	reclaimOut, err := runNoOverride(t, "bundle-reclaim",
		"--root", filepath.Join(".herd", "coordinator-resume", "shared-lock-transport"),
		"--manifest", filepath.Join(wtHerd, "retention-linked-cwd.json"),
		"--dry-run",
	)
	if err != nil {
		t.Fatalf("linked-cwd consumer must resolve the canonical shared lock without an env override: %v\n%s", err, reclaimOut)
	}
}

// TestBundleReclaimCLIRelativeRootAndManifest reproduces the PR814
// integration regression hermetically: the coordinator produces the
// retention manifest with BOTH --root and --out cwd-relative, then the
// consumer must accept the SAME cwd-relative --manifest from the same
// cwd — with NO HERD_CANONICAL_ROOT ambient dependency. Missing,
// outside-root, and symlink manifest destinations stay refused.
func TestBundleReclaimCLIRelativeRootAndManifest(t *testing.T) {
	repoRoot, bundleDir, bundleName := bundleManifestFixture(t)
	data, err := os.ReadFile(filepath.Join(bundleDir, bundleName))
	if err != nil {
		t.Fatal(err)
	}
	// A real registered linked worktree with its own owned state, exactly
	// the production layout (REL_O under the linked worktree).
	wt := filepath.Join(repoRoot, "reclaim-rel-wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", wt, "-b", "wt-rel-manifest").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	owned := filepath.Join(".herd", "coordinator-resume", "rel-transport")
	wtOwned := filepath.Join(wt, owned)
	if err := os.MkdirAll(wtOwned, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtOwned, "rel-transfer.bundle"), data, 0644); err != nil {
		t.Fatal(err)
	}
	runFrom := func(t *testing.T, cwd string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(buildHerd(t), args...)
		cmd.Dir = cwd
		env := os.Environ()
		filtered := env[:0]
		for _, e := range env {
			// The override must be ABSENT: default lock + root resolution
			// flows through the process cwd, as in production.
			if !strings.HasPrefix(e, "HERD_CANONICAL_ROOT=") {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Relative to the CANONICAL cwd, the manifest lives inside the owned
	// root exactly as in production: --out REL_O/owned-...json.
	manifestRel := filepath.Join("reclaim-rel-wt", owned, "owned-rel-retention.json")
	// The same file relative to the LINKED worktree cwd.
	manifestRelFromWt := filepath.Join(owned, "owned-rel-retention.json")

	t.Run("consumer accepts cwd-relative root and manifest from canonical cwd", func(t *testing.T) {
		// Producer: same shape as the live PR814 regression (--write with
		// relative root and relative out), from the canonical cwd.
		if out, err := runFrom(t, repoRoot, "bundle-manifest",
			"--root", filepath.Join("reclaim-rel-wt", owned),
			"--bundle", "rel-transfer.bundle",
			"--authority", "relative-path-integration",
			"--out", manifestRel,
			"--write"); err != nil {
			t.Fatalf("producer must accept cwd-relative root/out: %v\n%s", err, out)
		}
		if _, statErr := os.Lstat(filepath.Join(repoRoot, manifestRel)); statErr != nil {
			t.Fatalf("manifest must exist after --write: %v", statErr)
		}
		// Consumer: the SAME relative root and relative manifest.
		out, err := runFrom(t, repoRoot, "bundle-reclaim",
			"--root", filepath.Join("reclaim-rel-wt", owned),
			"--manifest", manifestRel,
			"--dry-run")
		if err != nil {
			t.Fatalf("consumer must accept the cwd-relative manifest the producer wrote: %v\n%s", err, out)
		}
	})

	t.Run("consumer accepts cwd-relative paths from the linked worktree cwd", func(t *testing.T) {
		out, err := runFrom(t, wt, "bundle-reclaim",
			"--root", owned,
			"--manifest", manifestRelFromWt,
			"--dry-run")
		if err != nil {
			t.Fatalf("consumer must accept cwd-relative paths from the linked cwd: %v\n%s", err, out)
		}
	})

	t.Run("missing manifest is refused", func(t *testing.T) {
		out, err := runFrom(t, repoRoot, "bundle-reclaim",
			"--root", filepath.Join("reclaim-rel-wt", owned),
			"--manifest", filepath.Join(owned, "no-such-retention.json"),
			"--dry-run")
		if err == nil {
			t.Fatalf("missing manifest must be refused: %s", out)
		}
	})

	t.Run("manifest outside the owned root is refused", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside-retention.json")
		if err := os.WriteFile(outside, []byte(`{"version":2,"authority":"x","bundles":[{"name":"rel-transfer.bundle","digest":"`+strings.Repeat("0", 64)+`","size":1,"mod_time_unix_nano":1}]}`), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := runFrom(t, repoRoot, "bundle-reclaim",
			"--root", filepath.Join("reclaim-rel-wt", owned),
			"--manifest", outside,
			"--dry-run")
		if err == nil || !strings.Contains(out, "not inside the owned root") {
			t.Fatalf("outside-root manifest must be refused: %v\n%s", err, out)
		}
	})

	t.Run("symlinked manifest is refused", func(t *testing.T) {
		link := filepath.Join(wtOwned, "linked-retention.json")
		if err := os.Symlink(filepath.Join(wtOwned, "owned-rel-retention.json"), link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		out, err := runFrom(t, repoRoot, "bundle-reclaim",
			"--root", filepath.Join("reclaim-rel-wt", owned),
			"--manifest", link,
			"--dry-run")
		if err == nil || !strings.Contains(out, "regular non-symlink") {
			t.Fatalf("symlinked manifest must be refused: %v\n%s", err, out)
		}
	})
}
