//go:build unix

package transfer

import (
	"fmt"
	"os"

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
