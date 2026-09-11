//go:build linux

package lock

import (
	"os"
	"path/filepath"
	"testing"
)

// Linux-soundness premise for the pinned-descriptor identity proof: unlinking
// a linked-but-open directory drops its fstat link count to 0, which is the
// orphan marker the identity gates rely on when a replacement reuses the
// pinned (st_dev, st_ino) — a same-path recreate routinely does exactly that
// on ext4/overlayfs.
func TestPinnedDirLinkCountDropsToZeroAfterUnlinkLinux(t *testing.T) {
	dir := tempDir(t)
	lockDir := filepath.Join(dir, "r")
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.Open(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if got := pinnedLinkCount(pinned); got == 0 {
		t.Fatalf("pinned live directory link count = 0: the identity proof is unsound here")
	}
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	if got := pinnedLinkCount(pinned); got != 0 {
		t.Fatalf("pinned directory link count after unlink = %d, want 0: inode reuse would defeat the identity proof", got)
	}
}
