//go:build unix

package transfer

import (
	"os"
	"syscall"
)

// fileLinkCount returns the directory entries referencing the inode, or 0
// when the platform cannot answer. A count other than 1 retains the bundle
// conservatively: hard-link substitution frees no bytes and can race the
// revalidator.
func fileLinkCount(st os.FileInfo) uint64 {
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || raw == nil {
		return 0
	}
	return uint64(raw.Nlink)
}
