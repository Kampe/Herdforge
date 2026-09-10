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
// Any mismatch restores the moved object to its original name (or a
// non-manifest-eligible recovery name when that name is taken again) and
// reports why. A foreign replacement is never deleted; data is never
// silently stranded — an unrestorable object stays in its quarantine
// directory and the recovery path is reported.
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
	removeQuarantineIfEmpty := func() {
		if entries, dirErr := os.ReadDir(quarantine); dirErr == nil && len(entries) == 0 {
			_ = os.Remove(quarantine)
		}
	}

	moved, lstatErr := os.Lstat(stagedPath)
	if lstatErr != nil || !os.SameFile(before, moved) {
		// The entry swapped between the final revalidation and the rename:
		// the moved object is a foreign replacement. Restore it — never
		// delete it.
		if err := unix.Renameat(int(qFile.Fd()), staged, int(dirFile.Fd()), name); err != nil {
			recovered := name + ".recovered-" + randomSuffix()
			if recoverErr := unix.Renameat(int(qFile.Fd()), staged, int(dirFile.Fd()), recovered); recoverErr != nil {
				// The object stays in the quarantine directory rather than
				// being stranded or destroyed; report the recovery path.
				return false, fmt.Sprintf("restored-after-quarantine-failed: foreign replacement preserved at %s (%v)", stagedPath, err)
			}
			removeQuarantineIfEmpty()
			return false, fmt.Sprintf("restored-after-quarantine: foreign replacement preserved at %s (%v)", recovered, err)
		}
		removeQuarantineIfEmpty()
		return false, "restored-after-quarantine: entry replaced between revalidation and deletion"
	}
	if fileLinkCount(moved) != 1 {
		if err := unix.Renameat(int(qFile.Fd()), staged, int(dirFile.Fd()), name); err != nil {
			return false, fmt.Sprintf("restored-after-quarantine-failed: %v", err)
		}
		removeQuarantineIfEmpty()
		return false, "restored-after-quarantine: hard-link-substitution-possible"
	}
	digest, err := fileContentDigest(stagedPath, before.Size())
	if err != nil {
		if rErr := unix.Renameat(int(qFile.Fd()), staged, int(dirFile.Fd()), name); rErr != nil {
			return false, fmt.Sprintf("restored-after-quarantine-failed: %v", rErr)
		}
		removeQuarantineIfEmpty()
		return false, fmt.Sprintf("restored-after-quarantine: content-identity-unknown: %v", err)
	}
	if digest != pinnedDigest {
		if rErr := unix.Renameat(int(qFile.Fd()), staged, int(dirFile.Fd()), name); rErr != nil {
			return false, fmt.Sprintf("restored-after-quarantine-failed: %v", rErr)
		}
		removeQuarantineIfEmpty()
		return false, "restored-after-quarantine: content-identity-mismatch-at-quarantine"
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
