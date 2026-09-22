//go:build darwin

package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArchiveClonePlusHardlinkIsUncertainNotCertain(t *testing.T) {
	root, wt := archiveFixture(t)
	payload := []byte("clone-plus-hardlink-payload")
	orig := filepath.Join(root, "extent-source")
	clone := filepath.Join(wt, "bin", "herd-clone")
	alias := filepath.Join(wt, "bin", "herd-clone-alias")
	if err := os.WriteFile(orig, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Clonefile(orig, clone, 0); err != nil {
		t.Skipf("clonefile unsupported here: %v", err)
	}
	if err := os.Link(clone, alias); err != nil {
		t.Fatal(err)
	}
	sum, _, err := hashFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, filepath.FromSlash(ArtifactArchiveDir))
	if _, err := writeObject(archive, orig, sum); err != nil {
		t.Fatal(err)
	}

	rep, err := ArchiveIgnored(ArchiveRequest{Root: root, Target: wt, Archive: archive, Act: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Fatalf("extent source must survive: %v", err)
	}
	if rep.PhysicalReclaimCertainBytes != 0 || rep.NetReclaim != 0 {
		t.Fatalf("clone+hardlink must not be certain physical reclaim: %+v", rep)
	}
	if rep.PhysicalReclaimUncertainBytes == 0 {
		t.Fatalf("last names of the clone inode should be uncertain, report=%+v", rep)
	}
	if rep.LogicalUnlinkedBytes == 0 {
		t.Fatal("logical unlink must still be reported")
	}
}
