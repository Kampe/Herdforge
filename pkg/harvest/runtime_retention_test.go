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
