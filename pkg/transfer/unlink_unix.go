//go:build unix

package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// removeDirRelative unlinks name via the pinned parent directory file
// descriptor, eliminating path-based parent-replacement races: even if the
// directory at the old path is swapped, the unlink operates on the original
// pinned directory inode.
func removeDirRelative(dirFile *os.File, name string) error {
	if dirFile == nil {
		return fmt.Errorf("pinned parent directory is not open")
	}
	if err := unix.Unlinkat(int(dirFile.Fd()), name, 0); err != nil {
		return fmt.Errorf("unlinkat %s: %w", name, err)
	}
	return nil
}

// quarantineAndRemove is the deletion primitive for a fully revalidated
// bundle entry. The pinned parent fd pins the directory inode but NOT the
// directory entry: a writer can replace `name` between the final
// revalidation and a name-based unlinkat, and the replacement would be
// unlinked instead of the reviewed object. This primitive therefore never
// unlinks by origin name:
//
//  1. An exclusive quarantine directory is created beside the entry (same
//     filesystem, only this reclaim pass knows its name).
//  2. renameat moves the entry into the quarantine atomically. The rename
//     binds the ENTRY: the origin name stops resolving, and the moved object
//     is pinned at the quarantine path where no other writer can reach it.
//  3. The moved object is re-proven against the reviewed identity at the
//     quarantine path — same inode, link count one, and a full-content
//     digest equal to the manifest-pinned digest — race-free now.
//  4. Only a proven-owned object is unlinked, from its quarantine path.
//
// Any mismatch restores the moved object to its original name with an
// ATOMIC NO-CLOBBER operation (linkat): a second concurrent origin writer
// inside the mismatch-to-restore window is never overwritten. When the
// origin name is occupied, the object moves to a non-manifest-eligible
// recovery name, again no-clobber; when every name is occupied it stays in
// its quarantine directory and the recovery path is reported. A foreign
// replacement is never deleted; data is never silently stranded or
// destroyed.
func quarantineAndRemove(dirFile *os.File, dir, name string, before os.FileInfo, pinnedDigest string) (bool, string) {
	if dirFile == nil {
		return false, "pinned parent directory is not open"
	}
	quarantine, err := os.MkdirTemp(dir, ".reclaim-quarantine-")
	if err != nil {
		return false, fmt.Sprintf("quarantine-unavailable: %v", err)
	}
	qFile, err := os.Open(quarantine)
	if err != nil {
		_ = os.Remove(quarantine)
		return false, fmt.Sprintf("quarantine-unavailable: %v", err)
	}
	defer qFile.Close()

	staged := fmt.Sprintf("staged-%s-%d", randomSuffix(), os.Getpid())
	if err := unix.Renameat(int(dirFile.Fd()), name, int(qFile.Fd()), staged); err != nil {
		_ = os.Remove(quarantine)
		return false, fmt.Sprintf("quarantine-rename-failed: %v", err)
	}
	stagedPath := filepath.Join(quarantine, staged)
	// Deterministic interleaving seam (bundle-flock-restore-727): fires once
	// with the origin name now free and the moved object pinned at the
	// quarantine path — the exact mismatch-to-restore window a second
	// concurrent origin writer would enter. Production leaves it nil.
	if interleaveHook != nil {
		interleaveHook("quarantined")
	}
	removeQuarantineIfEmpty := func() {
		if entries, dirErr := os.ReadDir(quarantine); dirErr == nil && len(entries) == 0 {
			_ = os.Remove(quarantine)
		}
	}
	// restoreNoClobber atomically restores the staged object to target
	// name without ever overwriting an occupant: linkat is a no-clobber
	// operation (EEXIST if the name is taken), so a second concurrent
	// origin writer's file can never be destroyed by a restore. On success
	// the staged entry is unlinked and the target path is reported. When
	// the origin name and every recovery name are occupied, the object
	// stays quarantined (never stranded, never destroyed) and "" is
	// reported.
	restoreNoClobber := func() (string, bool) {
		if err := unix.Linkat(int(qFile.Fd()), staged, int(dirFile.Fd()), name, 0); err != nil {
			if !os.IsExist(err) {
				return "", false
			}
		} else {
			_ = unix.Unlinkat(int(qFile.Fd()), staged, 0)
			removeQuarantineIfEmpty()
			return name, true
		}
		for attempt := 0; attempt < 8; attempt++ {
			recovered := name + ".recovered-" + randomSuffix()
			if err := unix.Linkat(int(qFile.Fd()), staged, int(dirFile.Fd()), recovered, 0); err != nil {
				if os.IsExist(err) {
					continue
				}
				return "", false
			}
			_ = unix.Unlinkat(int(qFile.Fd()), staged, 0)
			removeQuarantineIfEmpty()
			return recovered, true
		}
		return "", false
	}

	moved, lstatErr := os.Lstat(stagedPath)
	if lstatErr != nil || !os.SameFile(before, moved) {
		// The entry swapped between the final revalidation and the rename:
		// the moved object is a foreign replacement. Restore it — never
		// delete it, and never overwrite whatever a second writer placed
		// at the origin name inside this window.
		if restored, ok := restoreNoClobber(); ok {
			return false, fmt.Sprintf("restored-after-quarantine: foreign replacement preserved at %s", restored)
		}
		return false, fmt.Sprintf("restored-after-quarantine-failed: foreign replacement preserved at %s", stagedPath)
	}
	if fileLinkCount(moved) != 1 {
		if restored, ok := restoreNoClobber(); ok {
			return false, fmt.Sprintf("restored-after-quarantine: hard-link-substitution-possible, object preserved at %s", restored)
		}
		return false, "restored-after-quarantine-failed: hard-link-substitution-possible"
	}
	digest, err := fileContentDigest(stagedPath, before.Size())
	if err != nil {
		if restored, ok := restoreNoClobber(); ok {
			return false, fmt.Sprintf("restored-after-quarantine: content-identity-unknown, object preserved at %s: %v", restored, err)
		}
		return false, fmt.Sprintf("restored-after-quarantine-failed: content-identity-unknown, object preserved at %s: %v", stagedPath, err)
	}
	if digest != pinnedDigest {
		if restored, ok := restoreNoClobber(); ok {
			return false, fmt.Sprintf("restored-after-quarantine: content-identity-mismatch-at-quarantine, object preserved at %s", restored)
		}
		return false, "restored-after-quarantine-failed: content-identity-mismatch-at-quarantine, object preserved in quarantine"
	}
	if err := unix.Unlinkat(int(qFile.Fd()), staged, 0); err != nil {
		return false, fmt.Sprintf("quarantine-unlink-failed: %v", err)
	}
	removeQuarantineIfEmpty()
	return true, ""
}

func randomSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(buf[:])
}
