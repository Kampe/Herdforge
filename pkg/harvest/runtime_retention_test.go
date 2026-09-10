package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
)

func retentionFixture(t *testing.T) runtimeFixture {
	t.Helper()
	f := newRuntimeFixture(t)
	f.installer.retention = &RuntimeRetentionOptions{
		LiveOwner: func(context.Context, string) (RuntimeOwnerStatus, error) {
			return RuntimeOwnerAbsent, nil
		},
	}
	return f
}

func advanceRetentionRuntime(t *testing.T, f *runtimeFixture, label string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.source, "main.go"), []byte("package main\nfunc main() { println(\""+label+"\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, f.source, "add", "main.go")
	runtimeGit(t, f.source, "commit", "-m", label)
	f.newSHA = runtimeGit(t, f.source, "rev-parse", "HEAD")
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.newSHA)
	runtimeBuild(t, f.source)
	f.installer.Revision = f.newSHA
	if binding, err := f.installer.Install(context.Background()); err != nil || binding == nil || binding.Revision != f.newSHA {
		t.Fatalf("install %s: binding=%+v err=%v", label, binding, err)
	}
}

func TestRuntimeRetentionKeepsCurrentAndTwoDistinctReceiptBoundVersions(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	advanceRetentionRuntime(t, &f, "second")
	advanceRetentionRuntime(t, &f, "third")
	advanceRetentionRuntime(t, &f, "fourth")

	manifestBody, err := os.ReadFile(filepath.Join(f.root, ".herd", runtimeRetentionManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest RuntimeRetentionManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != runtimeRetentionManifestVersion || len(manifest.Previous) != 2 {
		t.Fatalf("manifest chain = %+v", manifest)
	}
	if manifest.Current.Path != "bin/herd" || filepath.IsAbs(manifest.Current.Path) {
		t.Fatalf("manifest current path is not repository-relative: %q", manifest.Current.Path)
	}
	backups, err := os.ReadDir(filepath.Join(f.root, ".herd", "runtime-previous"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("backup count = %d, want current plus two prior retention", len(backups))
	}
	journal, err := os.ReadFile(filepath.Join(f.root, ".herd", runtimeRetentionJournalName))
	if err != nil || !strings.Contains(string(journal), `"event":"removed"`) {
		t.Fatalf("journal missing verified removal: err=%v body=%s", err, journal)
	}
}

func TestRuntimeRetentionDuplicateInstallDoesNotConsumeHistorySlot(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadDir(filepath.Join(f.root, ".herd", "runtime-previous"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadDir(filepath.Join(f.root, ".herd", "runtime-previous"))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("duplicate install changed history slots: before=%d after=%d", len(first), len(second))
	}
}

func TestRuntimeRetentionUnknownAndOpenCandidatesRemain(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, ".herd", "runtime-previous")
	unknown := filepath.Join(dir, "unknown")
	open := filepath.Join(dir, "open")
	symlink := filepath.Join(dir, "symlink")
	changed := filepath.Join(dir, "changed")
	if err := os.WriteFile(unknown, []byte("unknown"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(open, []byte("open"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unknown, symlink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("before"), 0755); err != nil {
		t.Fatal(err)
	}
	owner := func(_ context.Context, path string) (RuntimeOwnerStatus, error) {
		if path == open {
			return RuntimeOwnerPresent, nil
		}
		if path == changed {
			if err := os.WriteFile(path, []byte("after"), 0755); err != nil {
				return RuntimeOwnerUnknown, err
			}
			return RuntimeOwnerAbsent, nil
		}
		return RuntimeOwnerUnknown, nil
	}
	manifest, err := f.installer.loadRetentionManifest(filepath.Join(f.root, ".herd", runtimeRetentionManifestName))
	if err != nil || manifest == nil {
		t.Fatalf("manifest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	report, err := f.installer.retireRuntimeBackupsWith(ctx, *manifest, filepath.Join(f.root, ".herd", runtimeRetentionJournalName), 16, defaultRuntimeRetentionBytes, owner)
	if err != nil || report.Held < 4 {
		t.Fatalf("unknown/open report=%+v err=%v", report, err)
	}
	for _, path := range []string{unknown, open, symlink, changed} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refused candidate disappeared: %s: %v", path, err)
		}
	}
}

func TestRuntimeRetentionInstallReturnsBindingWithMaintenanceFailure(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".herd", "runtime-previous", "unknown"), []byte("unknown"), 0755); err != nil {
		t.Fatal(err)
	}
	f.installer.retention.LiveOwner = func(context.Context, string) (RuntimeOwnerStatus, error) {
		return RuntimeOwnerUnknown, nil
	}
	advanceErr := func() error {
		if err := os.WriteFile(filepath.Join(f.source, "main.go"), []byte("package main\nfunc main() { println(\"maintenance\") }\n"), 0644); err != nil {
			return err
		}
		runtimeGit(t, f.source, "add", "main.go")
		runtimeGit(t, f.source, "commit", "-m", "maintenance")
		f.newSHA = runtimeGit(t, f.source, "rev-parse", "HEAD")
		runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.newSHA)
		runtimeBuild(t, f.source)
		f.installer.Revision = f.newSHA
		binding, err := f.installer.Install(context.Background())
		if binding == nil || err == nil || !strings.Contains(err.Error(), "installed binding preserved") {
			return err
		}
		return nil
	}
	if err := advanceErr(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRetentionTimeoutLeavesCandidateAndReportsPartial(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(f.root, ".herd", "runtime-previous", "timeout-unknown")
	if err := os.WriteFile(candidate, []byte("timeout"), 0755); err != nil {
		t.Fatal(err)
	}
	manifest, err := f.installer.loadRetentionManifest(filepath.Join(f.root, ".herd", runtimeRetentionManifestName))
	if err != nil || manifest == nil {
		t.Fatalf("manifest: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := f.installer.retireRuntimeBackupsWith(ctx, *manifest, filepath.Join(f.root, ".herd", runtimeRetentionJournalName), 16, defaultRuntimeRetentionBytes, nil)
	if err != nil || !report.Partial || report.Reason != "timeout" {
		t.Fatalf("timeout report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("timeout removed candidate: %v", err)
	}
}

func TestRuntimeRetentionRefusesMultiLinkCandidate(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, ".herd", "runtime-previous")
	candidate := filepath.Join(dir, "multilink")
	alias := filepath.Join(dir, "multilink-alias")
	if err := os.WriteFile(candidate, []byte("multi"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(candidate, alias); err != nil {
		t.Fatal(err)
	}
	manifest, err := f.installer.loadRetentionManifest(filepath.Join(f.root, ".herd", runtimeRetentionManifestName))
	if err != nil || manifest == nil {
		t.Fatalf("manifest: %v", err)
	}
	report, err := f.installer.retireRuntimeBackupsWith(context.Background(), *manifest, filepath.Join(f.root, ".herd", runtimeRetentionJournalName), 16, defaultRuntimeRetentionBytes, func(context.Context, string) (RuntimeOwnerStatus, error) {
		return RuntimeOwnerAbsent, nil
	})
	if err != nil || report.Held == 0 {
		t.Fatalf("multi-link report=%+v err=%v", report, err)
	}
	for _, path := range []string{candidate, alias} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("multi-link candidate changed: %s: %v", path, err)
		}
	}
}

func TestRuntimeInstallRefusesSharedLockAndDoesNotMutate(t *testing.T) {
	f := retentionFixture(t)
	common := runtimeGit(t, f.root, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(f.root, common)
	}
	shared := lock.NewDirLock(filepath.Join(common, SharedIntegrationLockName))
	if err := shared.Acquire(context.Background(), 0, "retention test holder"); err != nil {
		t.Fatal(err)
	}
	defer shared.Release()
	before, err := os.Stat(filepath.Join(f.root, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := f.installer.Install(ctx); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected shared-lock refusal, got %v", err)
	}
	after, err := os.Stat(filepath.Join(f.root, "bin", "herd"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lock refusal changed installed target: before=%v after=%v err=%v", before, after, err)
	}
}

// TestRuntimeRetentionRefusesRemovalWhenCurrentInstallChangedSinceCensus
// proves the just-in-time recheck at the deletion point of
// retireRuntimeBackupsWith: manifest.Current captured at one moment (the
// census) is later handed to a real second retention pass after the
// installed binary has genuinely moved on to a different revision/digest
// -- via two real, provenance-valid installs, never a hand-forged byte
// mismatch. The candidate itself is untouched (same path, same inode, same
// stat, single link, owned, not itself part of the stale manifest's
// Previous chain) so no other guard in the loop -- link count, ownership,
// containment, the candidate's own before/after Lstat recheck -- has any
// basis to hold it; only the current-binding digest recheck can and must
// refuse it.
func TestRuntimeRetentionRefusesRemovalWhenCurrentInstallChangedSinceCensus(t *testing.T) {
	f := retentionFixture(t)
	if _, err := f.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(f.root, ".herd", runtimeRetentionManifestName)
	staleManifest, err := f.installer.loadRetentionManifest(manifestPath)
	if err != nil || staleManifest == nil {
		t.Fatalf("stale manifest: %v", err)
	}
	staleCurrentDigest := staleManifest.Current.Digest

	// A second real install genuinely changes the installed binary's
	// revision and digest -- staleManifest.Current now names a binding
	// that is no longer what is actually installed at bin/herd.
	advanceRetentionRuntime(t, &f, "third")
	installedNow, err := f.installer.inspect(filepath.Join(f.root, "bin", "herd"))
	if err != nil {
		t.Fatalf("inspect installed: %v", err)
	}
	if installedNow.Digest == staleCurrentDigest {
		t.Fatal("test setup failure: installed digest did not actually change")
	}

	dir := filepath.Join(f.root, ".herd", "runtime-previous")
	candidate := filepath.Join(dir, "jit-guard-candidate")
	if err := os.WriteFile(candidate, []byte("otherwise fully eligible backup content"), 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	// The current-install recheck fires on the destructive path, which under
	// the positive allowlist requires the candidate to match a displaced
	// binding with full recorded identity. A stray unbound file is held long
	// before the recheck, so register the candidate as displaced.
	candidateDigest, err := runtimeFileDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	staleManifest.Displaced = append(staleManifest.Displaced, RuntimeRetentionBinding{
		RuntimeBinding:  RuntimeBinding{Revision: "displaced-test", Digest: candidateDigest, Executable: "bin/herd"},
		Path:            ".herd/runtime-previous/jit-guard-candidate",
		Size:            before.Size(),
		ModTimeUnixNano: before.ModTime().UnixNano(),
	})

	journalPath := filepath.Join(f.root, ".herd", runtimeRetentionJournalName)
	report, err := f.installer.retireRuntimeBackupsWith(context.Background(), *staleManifest, journalPath, 16, defaultRuntimeRetentionBytes,
		func(context.Context, string) (RuntimeOwnerStatus, error) { return RuntimeOwnerAbsent, nil })
	if err != nil {
		t.Fatalf("retireRuntimeBackupsWith: %v", err)
	}
	if report.Removed != 0 {
		t.Fatalf("stale current binding must remove nothing, report=%+v", report)
	}
	if report.Errors == 0 || report.Reason != "current-install-changed" {
		t.Fatalf("expected a current-install-changed refusal, got report=%+v", report)
	}

	after, err := os.Lstat(candidate)
	if err != nil {
		t.Fatalf("candidate must be retained on disk, stat failed: %v", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatalf("candidate identity changed across a refused pass: before=%+v after=%+v", before, after)
	}

	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), `"path":"jit-guard-candidate","logical_bytes"`) || strings.Contains(string(journal), `"event":"removed","path":"jit-guard-candidate"`) {
		t.Fatalf("journal must not record a destructive success for the held candidate: %s", journal)
	}
}

func TestRuntimeRetentionHoldsUnboundCleanCandidate(t *testing.T) {
	f := retentionFixture(t)
	advanceRetentionRuntime(t, &f, "second")
	manifestPath, journalPath := f.installer.retentionPaths()
	manifest, err := f.installer.loadRetentionManifest(manifestPath)
	if err != nil || manifest == nil {
		t.Fatalf("manifest load: %v", err)
	}
	// A same-owner, single-link, regular, digest-clean file that matches no
	// manifest binding must be held as unknown — never removed.
	dir := filepath.Join(f.root, ".herd", "runtime-previous")
	stray := filepath.Join(dir, "stray-clean")
	if err := os.WriteFile(stray, []byte("unbound clean candidate body"), 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(stray)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.installer.retireRuntimeBackupsWith(context.Background(), *manifest, journalPath, 16, defaultRuntimeRetentionBytes,
		func(context.Context, string) (RuntimeOwnerStatus, error) { return RuntimeOwnerAbsent, nil })
	if err != nil {
		t.Fatalf("retireRuntimeBackupsWith: %v", err)
	}
	if report.Removed != 0 {
		t.Fatalf("unbound candidate must never be removed: %+v", report)
	}
	if report.Held != 1 {
		t.Fatalf("unbound candidate must be held exactly once: %+v", report)
	}
	found := false
	for _, event := range report.Events {
		if event.Path == "stray-clean" && event.Reason == "unbound-candidate-not-in-retention-manifest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unbound-candidate hold event: %+v", report.Events)
	}
	after, err := os.Lstat(stray)
	if err != nil {
		t.Fatalf("unbound candidate must remain on disk: %v", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatalf("unbound candidate identity changed across the pass")
	}
}

func TestRuntimeRetentionRemovesOnlyDisplacedAllowlistedBinding(t *testing.T) {
	f := retentionFixture(t)
	advanceRetentionRuntime(t, &f, "second")
	advanceRetentionRuntime(t, &f, "third")
	manifestPath, journalPath := f.installer.retentionPaths()
	manifest, err := f.installer.loadRetentionManifest(manifestPath)
	if err != nil || manifest == nil {
		t.Fatalf("manifest load: %v", err)
	}
	if len(manifest.Previous) != 2 {
		t.Fatalf("setup: expected two kept priors, got %+v", manifest)
	}
	// Simulate the rebind that displaces the oldest kept binding: the
	// Previous list shrinks to its newest entry and the displaced binding
	// moves to the removal allowlist with its full recorded identity.
	displaced := manifest.Previous[1]
	manifest.Previous = manifest.Previous[:1]
	manifest.Displaced = []RuntimeRetentionBinding{displaced}
	dir := filepath.Join(f.root, ".herd", "runtime-previous")
	report, err := f.installer.retireRuntimeBackupsWith(context.Background(), *manifest, journalPath, 16, defaultRuntimeRetentionBytes,
		func(context.Context, string) (RuntimeOwnerStatus, error) { return RuntimeOwnerAbsent, nil })
	if err != nil {
		t.Fatalf("retireRuntimeBackupsWith: %v", err)
	}
	if report.Removed != 1 || report.Protected != 1 || report.Held != 0 {
		t.Fatalf("exactly the displaced binding must be removed: report=%+v", report)
	}
	removed := filepath.Join(f.root, displaced.Path)
	if _, err := os.Lstat(removed); !os.IsNotExist(err) {
		t.Fatalf("displaced binding must be removed: %v", err)
	}
	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("kept chain must survive: remaining=%d", len(remaining))
	}
}
