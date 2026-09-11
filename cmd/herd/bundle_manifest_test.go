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
