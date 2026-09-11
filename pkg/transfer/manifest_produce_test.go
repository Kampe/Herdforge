package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// extraBundle copies the verified fixture bundle under a new exact name —
// a valid, verifyable, regular single-linked bundle for multi-entry tests.
func extraBundle(f reclaimFixture, name string) string {
	path := filepath.Join(f.bundleDir, name)
	data := readAll(f.t, f.bundlePath)
	if err := os.WriteFile(path, data, 0644); err != nil {
		f.t.Fatal(err)
	}
	return name
}

// TestProduceManifestPublicationIsAtomicAndNoReplace proves the manifest is
// published through a completely written, synced same-parent temporary file
// and a no-replace atomic publication: write/sync failure leaves no partial
// final artifact, a destination materialized concurrently is never
// clobbered, and cleanup touches only the owned temporary file.
func TestProduceManifestPublicationIsAtomicAndNoReplace(t *testing.T) {
	f := produceFixtureSetup(t)
	out := filepath.Join(f.bundleDir, "retention-manifest.json")
	writePass := func() (ProduceReport, error) {
		return ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
			o.Write = true
			o.Out = out
		}))
	}
	tempLeftovers := func() []string {
		entries, _ := os.ReadDir(f.bundleDir)
		var found []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".herd-bundle-manifest-") && strings.HasSuffix(e.Name(), ".tmp") {
				found = append(found, e.Name())
			}
		}
		return found
	}

	t.Run("write failure leaves no partial artifact", func(t *testing.T) {
		produceTempWriteHook = func(tempPath string) error {
			return fmt.Errorf("injected sync failure")
		}
		defer func() { produceTempWriteHook = nil }()
		if _, err := writePass(); err == nil || !contains(err.Error(), "temporary write aborted") {
			t.Fatalf("injected write failure must fail the pass: %v", err)
		}
		if _, statErr := os.Lstat(out); statErr == nil {
			t.Fatal("no final artifact may exist after a write/sync failure")
		}
		if left := tempLeftovers(); len(left) != 0 {
			t.Fatalf("owned temporary file must be cleaned up: %v", left)
		}
	})

	t.Run("concurrent destination is never clobbered", func(t *testing.T) {
		producePublishHook = func(tempPath, finalPath string) {
			if err := os.WriteFile(finalPath, []byte("concurrent-owner\n"), 0644); err != nil {
				t.Errorf("concurrent creation failed: %v", err)
			}
		}
		defer func() { producePublishHook = nil }()
		if _, err := writePass(); err == nil || !contains(err.Error(), "never overwritten") {
			t.Fatalf("concurrent destination must be refused, not clobbered: %v", err)
		}
		data, readErr := os.ReadFile(out)
		if readErr != nil || string(data) != "concurrent-owner\n" {
			t.Fatalf("concurrent owner must survive byte-identical: %q %v", data, readErr)
		}
		if left := tempLeftovers(); len(left) != 0 {
			t.Fatalf("owned temporary file must be cleaned up after refusal: %v", left)
		}
	})

	t.Run("pre-existing destination is refused without cleanup side effects", func(t *testing.T) {
		if err := os.WriteFile(out, []byte("pre-existing\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := writePass(); err == nil || !contains(err.Error(), "never overwritten") {
			t.Fatalf("existing destination must be refused: %v", err)
		}
		data, readErr := os.ReadFile(out)
		if readErr != nil || string(data) != "pre-existing\n" {
			t.Fatalf("existing artifact must survive byte-identical: %q %v", data, readErr)
		}
		if left := tempLeftovers(); len(left) != 0 {
			t.Fatalf("owned temporary file must be cleaned up after refusal: %v", left)
		}
	})
}

// TestProduceManifestRefusesRootAndParentReplacement pins owned-root and
// output-parent identity at capture and revalidates them at publication, so
// a directory swapped in between capture and publish is deterministically
// refused instead of receiving manifest authority.
func TestProduceManifestRefusesRootAndParentReplacement(t *testing.T) {
	f := produceFixtureSetup(t)
	sub := filepath.Join(f.bundleDir, "manifests")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("output parent swap is refused", func(t *testing.T) {
		out := filepath.Join(sub, "retention-manifest.json")
		producePublishHook = func(tempPath, finalPath string) {
			// Replace the output parent with a fresh directory between
			// identity capture and publication.
			away := sub + ".swapped-away"
			if err := os.Rename(sub, away); err != nil {
				t.Errorf("parent swap setup failed: %v", err)
				return
			}
			defer os.RemoveAll(away)
			if err := os.MkdirAll(sub, 0755); err != nil {
				t.Errorf("parent swap setup failed: %v", err)
			}
		}
		defer func() { producePublishHook = nil }()
		_, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
			o.Write = true
			o.Out = out
		}))
		if err == nil || !contains(err.Error(), "replaced between identity capture and publication") {
			t.Fatalf("parent replacement must be refused: %v", err)
		}
	})

	t.Run("owned root swap is refused", func(t *testing.T) {
		// Out in a subdirectory isolates the root-identity revalidation:
		// the output parent inside the swapped root does not itself prove
		// or disprove the pinned root inode.
		out := filepath.Join(sub, "retention-manifest-root.json")
		producePublishHook = func(tempPath, finalPath string) {
			// Swap the whole owned root between capture and publication.
			away := f.bundleDir + ".swapped-away"
			if err := os.Rename(f.bundleDir, away); err != nil {
				t.Errorf("root swap setup failed: %v", err)
				return
			}
			if err := os.MkdirAll(f.bundleDir, 0755); err != nil {
				t.Errorf("root swap setup failed: %v", err)
			}
		}
		defer func() { producePublishHook = nil }()
		_, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
			o.Write = true
			o.Out = out
		}))
		// Restore the original root for later sub-tests and hygiene.
		fresh := f.bundleDir + ".fresh"
		if err := os.Rename(f.bundleDir, fresh); err == nil {
			if err := os.Rename(f.bundleDir+".swapped-away", f.bundleDir); err == nil {
				os.RemoveAll(fresh)
			}
		}
		if err == nil || !contains(err.Error(), "replaced between identity capture and publication") {
			t.Fatalf("owned-root replacement must be refused: %v", err)
		}
	})
}

// TestProduceManifestLoaderContract pins the producer-to-loader contract for
// every field the v2 consumer validates: the producer refuses metadata its
// own consumer would reject, before any document is emitted.
func TestProduceManifestLoaderContract(t *testing.T) {
	f := produceFixtureSetup(t)

	// Producer side: non-positive integer-nanosecond modification time is
	// refused at production.
	if err := os.Chtimes(f.bundlePath, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Skipf("mtime pinning unavailable: %v", err)
	}
	_, err := ProduceRetentionManifest(context.Background(), produceOpts(f, nil))
	if err == nil || !contains(err.Error(), "invalid modification time") {
		t.Fatalf("non-positive mtime must be refused before emission: %v", err)
	}

	// Loader side: the same contract holds at consumption, so a producer
	// bug could never silently produce an unloadable manifest.
	inside := filepath.Join(f.bundleDir, "retention-manifest-zero.json")
	doc := fmt.Sprintf(`{"version":2,"authority":"forge-orchestrator-test","bundles":[{"name":%q,"digest":"0000000000000000000000000000000000000000000000000000000000000000","size":1,"mod_time_unix_nano":0}]}`, filepath.Base(f.bundlePath))
	if err := os.WriteFile(inside, []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRetentionManifest(f.bundleDir, inside); err == nil || !contains(err.Error(), "must pin a modification time") {
		t.Fatalf("loader must reject the same entry the producer refuses: %v", err)
	}
}

// TestProduceManifestBoundsAndOverflow proves the explicit selection is
// still bounded at product level: the entry budget reuses the reclaim
// enumeration budget, the byte budget reuses the reclaim byte budget, and
// byte accumulation refuses overflow instead of wrapping.
func TestProduceManifestBoundsAndOverflow(t *testing.T) {
	f := produceFixtureSetup(t)
	origEntries, origBytes := produceMaxEntries, produceMaxTotalBytes
	defer func() {
		produceMaxEntries, produceMaxTotalBytes = origEntries, origBytes
	}()
	second := extraBundle(f, "second-transfer.bundle")

	produceMaxEntries = 1
	_, err := ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{filepath.Base(f.bundlePath), second}
	}))
	if err == nil || !contains(err.Error(), "exceed the manifest bound of 1") {
		t.Fatalf("entry bound must refuse: %v", err)
	}

	produceMaxEntries = origEntries
	produceMaxTotalBytes = f.bundleSize - 1
	if f.bundleSize <= 1 {
		t.Fatalf("fixture bundle too small for the byte-bound test: %d", f.bundleSize)
	}
	_, err = ProduceRetentionManifest(context.Background(), produceOpts(f, nil))
	if err == nil || !contains(err.Error(), "byte budget") {
		t.Fatalf("byte bound must refuse: %v", err)
	}

	// Overflow arithmetic: a second bundle pushes the accumulation past the
	// budget; the guard must refuse rather than wrap or underflow.
	produceMaxTotalBytes = 2*f.bundleSize - 1
	_, err = ProduceRetentionManifest(context.Background(), produceOpts(f, func(o *ProduceOptions) {
		o.Bundles = []string{filepath.Base(f.bundlePath), second}
	}))
	if err == nil || !contains(err.Error(), "byte budget") {
		t.Fatalf("byte accumulation must refuse past the budget: %v", err)
	}
}
