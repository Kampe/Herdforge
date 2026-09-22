package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanApplySourceReplacedRefusesAndKeeps(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	planRep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"ref":"FAC-843","replaced":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true, Plan: planRep.Plan})
	if err == nil || !strings.Contains(err.Error(), "replaced after plan") {
		t.Fatalf("want replacement drift, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("replaced source must be kept")
	}
}

func TestPlanApplyExternalAliasRefusesAndKeeps(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, "bin", "herd")
	planRep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, filepath.Join(root, "external-after-plan")); err != nil {
		t.Fatal(err)
	}
	_, err = ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Act: true, Plan: planRep.Plan})
	if err == nil || !strings.Contains(err.Error(), "gained external alias") {
		t.Fatalf("want alias drift, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("source must be kept after alias drift")
	}
}

func TestPlanApplyCorruptObjectRefusesAndKeeps(t *testing.T) {
	root, wt := archiveFixture(t)
	src := filepath.Join(wt, ".herd", "receipts", "FAC-843.json")
	planRep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, pf := range planRep.Plan.Files {
		if pf.Path == ".herd/receipts/FAC-843.json" {
			digest = pf.Digest
		}
	}
	if digest == "" {
		t.Fatal("plan missing receipt")
	}
	archive := filepath.Join(root, filepath.FromSlash(ArtifactArchiveDir))
	dst := objectPath(archive, digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("corrupt-after-plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Archive: archive, Act: true, Plan: planRep.Plan})
	if err == nil || !strings.Contains(err.Error(), "different digest") {
		t.Fatalf("want corrupt object refusal, got %v", err)
	}
	if _, stat := os.Stat(src); stat != nil {
		t.Fatal("source must be kept until verified archive exists")
	}
}
