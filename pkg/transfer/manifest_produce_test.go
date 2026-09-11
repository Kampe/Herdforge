package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// produceFixtureSetup mirrors reclaimFixtureSetup: a hermetic temp git repo
// whose owned .herd bundle directory holds one real, verified transfer
// bundle.
func produceFixtureSetup(t *testing.T) reclaimFixture {
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

	bundleDir := filepath.Join(repoRoot, ".herd", "coordinator-resume", "completion-recovery-test")
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(bundleDir, "eligible-transfer.bundle")
	cmd := exec.Command("git", "-C", repoRoot, "bundle", "create", bundlePath, "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	st, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	f := reclaimFixture{t: t, repoRoot: repoRoot, bundleDir: bundleDir, bundlePath: bundlePath, bundleSize: st.Size()}
	f.lockDir = filepath.Join(repoRoot, ".git", "herd-shared-checkout.lock.d")
	return f
}

func produceOpts(f reclaimFixture, mutate func(*ProduceOptions)) ProduceOptions {
	o := ProduceOptions{
		RepoRoot:  f.repoRoot,
		Root:      f.bundleDir,
		Authority: "forge-orchestrator-test",
		Bundles:   []string{filepath.Base(f.bundlePath)},
		LockDir:   f.lockDir,
		LockWait:  5 * time.Second,
	}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

// TestProduceManifestCapturesExactContentIdentity proves the produced
// entries reproduce the identity the reclaim pass re-checks at deletion:
// streaming sha256, exact size, and integer nanosecond modification time.
func TestProduceManifestCapturesExactContentIdentity(t *testing.T) {
	f := produceFixtureSetup(t)
	report, err := ProduceRetentionManifest(context.Background(), produceOpts(f, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Bundles) != 1 {
		t.Fatalf("expected one entry, got %+v", report.Bundles)
	}
	entry := report.Bundles[0]
	st, err := os.Stat(f.bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(readAll(t, f.bundlePath))
	if entry.Digest != hex.EncodeToString(want[:]) {
		t.Fatalf("digest mismatch: %s", entry.Digest)
	}
	if entry.Size != st.Size() || entry.Size <= 0 {
		t.Fatalf("size mismatch: %d", entry.Size)
	}
	if entry.ModTimeUnixNano != st.ModTime().UnixNano() || entry.ModTimeUnixNano <= 0 {
		t.Fatalf("integer nanosecond identity mismatch: %d vs %d", entry.ModTimeUnixNano, st.ModTime().UnixNano())
	}
	if entry.Name != filepath.Base(f.bundlePath) {
		t.Fatalf("name mismatch: %q", entry.Name)
	}
	// The produced document must satisfy the reclaim consumer's loader.
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	if err := os.WriteFile(manifestPath, report.ManifestJSON, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRetentionManifest(f.bundleDir, manifestPath); err == nil {
		t.Fatal("loader accepted a manifest outside the owned root; parity broken")
	} else if !contains(err.Error(), "not inside the owned root") {
		t.Fatalf("unexpected loader error: %v", err)
	}
	inside := filepath.Join(f.bundleDir, "retention-manifest-produced.json")
	if err := os.WriteFile(inside, report.ManifestJSON, 0644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadRetentionManifest(f.bundleDir, inside)
	if err != nil {
		t.Fatalf("loader rejected the produced manifest: %v", err)
	}
	if m.Version != 2 || len(m.Bundles) != 1 || m.Authority != "forge-orchestrator-test" {
		t.Fatalf("round-trip mismatch: %+v", m)
	}
}

// TestProduceManifestDryRunCreatesNothing proves the default pass only
// describes.
func TestProduceManifestDryRunCreatesNothing(t *testing.T) {
	f := produceFixtureSetup(t)
	before := dirEntries(t, f.bundleDir)
	report, err := ProduceRetentionManifest(context.Background(), produceOpts(f, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Written || len(report.ManifestJSON) == 0 {
		t.Fatalf("dry-run must only describe: %+v", report)
	}
	if string(report.ManifestJSON) == "" || !contains(string(report.ManifestJSON), "\"version\": 2") {
		t.Fatalf("dry run must describe the exact v2 document: %q", report.ManifestJSON)
	}
	if after := dirEntries(t, f.bundleDir); !sameEntries(before, after) {
		t.Fatalf("dry run mutated the root: before=%v after=%v", before, after)
	}
}

// TestProduceManifestWriteAtomicNoOverwrite covers the write path and its
// refusal surfaces.
func TestProduceManifestWriteAtomicNoOverwrite(t *testing.T) {
	f := produceFixtureSetup(t)
	out := filepath.Join(f.bundleDir, "retention-manifest.json")
	report, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Write = true
		o.Out = out
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Written {
		t.Fatalf("expected a written manifest: %+v", report)
	}
	st, err := os.Lstat(out)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("output must be a regular non-symlink file: %v", err)
	}
	// Existing artifact is never overwritten.
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Write = true
		o.Out = out
	})); err == nil || !contains(err.Error(), "never overwritten") {
		t.Fatalf("overwrite must be refused: %v", err)
	}
	// Destination outside the owned root is refused.
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Write = true
		o.Out = filepath.Join(t.TempDir(), "m.json")
	})); err == nil || !contains(err.Error(), "not inside the owned root") {
		t.Fatalf("outside-root destination must be refused: %v", err)
	}
	// Symlink destination is never followed.
	symlink := filepath.Join(f.bundleDir, "symlink-manifest.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "target.json"), symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Write = true
		o.Out = symlink
	})); err == nil || !contains(err.Error(), "never overwritten") {
		t.Fatalf("symlink destination must be refused: %v", err)
	}
}

// TestProduceManifestRefusals covers the fail-closed guards: missing bundle,
// non-.bundle name, duplicates, symlinked bundle, hard-linked bundle,
// verify failure, and unretained tips. No partial manifest may be produced.
func TestProduceManifestRefusals(t *testing.T) {
	f := produceFixtureSetup(t)
	base := filepath.Base(f.bundlePath)

	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"missing-transfer.bundle"}
	})); err == nil || !contains(err.Error(), "missing-transfer.bundle") {
		t.Fatalf("missing bundle must fail closed: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"../escape.bundle"}
	})); err == nil || !contains(err.Error(), "exact .bundle filename") {
		t.Fatalf("path escape must be refused: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{base, base}
	})); err == nil || !contains(err.Error(), "duplicate") {
		t.Fatalf("duplicates must be refused: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = nil
	})); err == nil || !contains(err.Error(), "never selected implicitly") {
		t.Fatalf("empty selection must be refused: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Authority = " "
	})); err == nil || !contains(err.Error(), "--authority is required") {
		t.Fatalf("blank authority must be refused: %v", err)
	}

	// Symlinked bundle name is refused, never followed.
	symlinkBundle := filepath.Join(f.bundleDir, "symlinked.bundle")
	if err := os.Symlink(f.bundlePath, symlinkBundle); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"symlinked.bundle"}
	})); err == nil || !contains(err.Error(), "not a regular non-symlink") {
		t.Fatalf("symlinked bundle must be refused: %v", err)
	}

	// Hard-linked bundle is refused (substitution guard parity with reclaim).
	hardlinkBundle := filepath.Join(f.bundleDir, "hardlinked.bundle")
	if err := os.Link(f.bundlePath, hardlinkBundle); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"hardlinked.bundle"}
	})); err == nil || !contains(err.Error(), "hard-link-substitution-possible") {
		t.Fatalf("hard-linked bundle must be refused: %v", err)
	}

	// Garbage content fails git bundle verify.
	garbage := filepath.Join(f.bundleDir, "garbage.bundle")
	if err := os.WriteFile(garbage, []byte("not a bundle"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"garbage.bundle"}
	})); err == nil || !contains(err.Error(), "bundle-verify-failed") {
		t.Fatalf("garbage bundle must be refused: %v", err)
	}

	// A bundle whose tips are not retained by canonical refs is refused.
	cmd := exec.Command("sh", "-c", `git init -q -b main other && cd other && git config user.email o@o && git config user.name o && echo unique > u.txt && git add . && git commit -q -m unique && git bundle create "$ORPHAN" main`)
	cmd.Dir = f.repoRoot
	cmd.Env = append(os.Environ(), "ORPHAN="+filepath.Join(f.bundleDir, "orphan.bundle"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("orphan bundle create: %v\n%s", err, out)
	}
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{"orphan.bundle"}
	})); err == nil || !contains(err.Error(), "tip") {
		t.Fatalf("orphan-tip bundle must be refused: %v", err)
	}
}

// TestProduceManifestNoBundleDeleted proves the production path never
// modifies or deletes the corpus — the bundle survives byte-identical.
func TestProduceManifestNoBundleDeleted(t *testing.T) {
	f := produceFixtureSetup(t)
	before := readAll(t, f.bundlePath)
	out := filepath.Join(f.bundleDir, "retention-manifest.json")
	if _, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Write = true
		o.Out = out
	})); err != nil {
		t.Fatal(err)
	}
	if after := readAll(t, f.bundlePath); string(before) != string(after) {
		t.Fatal("bundle content changed during manifest production")
	}
	if _, err := os.Stat(f.bundlePath); err != nil {
		t.Fatalf("bundle must survive production: %v", err)
	}
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func sameEntries(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	bm := map[string]bool{}
	for _, n := range b {
		bm[n] = true
	}
	for _, n := range a {
		if !bm[n] {
			return false
		}
	}
	return true
}
