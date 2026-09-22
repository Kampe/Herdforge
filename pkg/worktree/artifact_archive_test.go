package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func silenceLiveProbes(t *testing.T) {
	t.Helper()
	prevOwner, prevProc := liveOwnerProbe, processHolderProbe
	liveOwnerProbe = func(string) error { return nil }
	processHolderProbe = func([]string) error { return nil }
	t.Cleanup(func() {
		liveOwnerProbe, processHolderProbe = prevOwner, prevProc
	})
}

func archiveFixture(t *testing.T) (root, wt string) {
	t.Helper()
	silenceLiveProbes(t)
	root = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("graph.db\nbin/\nherd\n.herd/receipts/\nTASK-CONTEXT.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	wt = filepath.Join(root, ".herd", "worktrees", "fac-843")
	git("worktree", "add", "-q", wt, "HEAD")
	if err := os.MkdirAll(filepath.Join(wt, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".herd", "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "graph.db"), []byte("graph-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "bin", "herd"), []byte("bin-herd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".herd", "receipts", "FAC-843.json"), []byte(`{"ref":"FAC-843"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, wt
}

func TestArchiveIgnoredDryRunRemovesNothing(t *testing.T) {
	root, wt := archiveFixture(t)
	rep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Removed != 0 || rep.Act {
		t.Fatalf("dry-run removed %d act=%v", rep.Removed, rep.Act)
	}
	if len(rep.Entries) < 2 {
		t.Fatalf("expected ignored receipts and derived files, got %+v", rep.Entries)
	}
	if _, err := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); err != nil {
		t.Fatal("dry-run must keep the receipt")
	}
}

func TestArchiveIgnoredActArchivesReceiptThenRemoves(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	want, _, err := hashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Removed == 0 {
		t.Fatal("act must remove archived sources")
	}
	var sawReceipt bool
	for _, e := range rep.Entries {
		if e.Kind == ArtifactReceipt && e.Path == ".herd/receipts/FAC-843.json" {
			sawReceipt = true
			if e.Digest != want {
				t.Fatalf("receipt digest %s want %s", e.Digest, want)
			}
			obj := objectPath(filepath.Join(root, filepath.FromSlash(ArtifactArchiveDir)), e.Digest)
			got, _, err := hashFile(obj)
			if err != nil {
				t.Fatalf("archived receipt missing: %v", err)
			}
			if got != want {
				t.Fatalf("archived receipt digest %s want %s", got, want)
			}
		}
	}
	if !sawReceipt {
		t.Fatalf("manifest omitted receipt: %+v", rep.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source receipt must be removed only after archive readback")
	}
	if rep.Manifest == "" {
		t.Fatal("act must write a manifest")
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rep.Manifest)))
	if err != nil {
		t.Fatal(err)
	}
	var man ArtifactManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Version != 1 || len(man.Entries) != len(rep.Entries) {
		t.Fatalf("manifest %+v", man)
	}
}

func TestArchiveIgnoredDriftRefusesAndKeepsSource(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	t.Cleanup(func() { afterArchiveHook = nil })
	afterArchiveHook = func(path string) {
		if filepath.Base(path) == "FAC-843.json" {
			_ = os.WriteFile(path, []byte(`{"ref":"FAC-843","tampered":true}`), 0o644)
		}
	}
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("want drift refusal, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("drift must keep the source receipt")
	}
}

func TestArchiveIgnoredActiveOwnerRefuses(t *testing.T) {
	root, wt := archiveFixture(t)
	body := []byte(`{"task_ref":"FAC-843","expires_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`)
	if err := os.WriteFile(filepath.Join(wt, "TASK-CONTEXT.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true, Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "active owner") {
		t.Fatalf("want active owner refusal, got %v", err)
	}
	if _, stat := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); stat != nil {
		t.Fatal("owner refusal must keep the receipt")
	}
}

func TestClassifyArtifactPathReceipts(t *testing.T) {
	if ClassifyArtifactPath(".herd/receipts/FAC-843.json") != ArtifactReceipt {
		t.Fatal("receipt")
	}
	if ClassifyArtifactPath("bin/herd") != ArtifactDerived {
		t.Fatal("derived")
	}
}

func TestArchiveMustSitOutsideTarget(t *testing.T) {
	root, wt := archiveFixture(t)
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Archive: filepath.Join(wt, "inside"), Act: true})
	if err == nil || !strings.Contains(err.Error(), "must be outside target") {
		t.Fatalf("want archive-outside-target refusal, got %v", err)
	}
	if _, stat := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); stat != nil {
		t.Fatal("source kept")
	}
}

func TestArchiveObjectCollisionRejectsDifferentBytes(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	sum, _, err := hashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, filepath.FromSlash(ArtifactArchiveDir))
	dst := objectPath(archive, sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("not-the-receipt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err == nil || !strings.Contains(err.Error(), "different digest") {
		t.Fatalf("want object collision refusal, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("collision must keep the source")
	}
}

func TestArchiveInterruptionKeepsSources(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	t.Cleanup(func() { beforeRemoveHook = nil })
	beforeRemoveHook = func() error { return errors.New("injected interrupt") }
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err == nil || !strings.Contains(err.Error(), "sources kept") {
		t.Fatalf("want interruption refusal, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("interrupted act must keep the source receipt")
	}
}

func TestArchiveUnknownProcessFailsClosed(t *testing.T) {
	root, wt := archiveFixture(t)
	processHolderProbe = func([]string) error { return errors.New("holders unknown") }
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err == nil || !strings.Contains(err.Error(), "holders unknown") {
		t.Fatalf("want process fail-closed, got %v", err)
	}
	if _, stat := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); stat != nil {
		t.Fatal("unknown process must keep the source")
	}
}

func TestArchiveHerdrCwdFailsClosed(t *testing.T) {
	root, wt := archiveFixture(t)
	liveOwnerProbe = func(string) error { return errors.New("herdr cwd owns target") }
	_, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true})
	if err == nil || !strings.Contains(err.Error(), "herdr cwd") {
		t.Fatalf("want herdr cwd refusal, got %v", err)
	}
}

func TestArchivePreservesCanonicalReceiptReferences(t *testing.T) {
	root, wt := archiveFixture(t)
	canon := filepath.Join(root, ".herd", "receipts", "FAC-843.json")
	if err := os.MkdirAll(filepath.Dir(canon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canon, []byte(`{"canonical":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refuseCanonicalReceipt(root, wt, canon); err == nil {
		t.Fatal("canonical receipt path must be refused")
	}
	if err := refuseCanonicalReceipt(root, wt, filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); err != nil {
		t.Fatalf("worktree-local receipt is archivable: %v", err)
	}
}
