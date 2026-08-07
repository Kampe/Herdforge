//go:build linux

package verifier

import (
	"io/fs"
	"syscall"
)

// fileID is a kernel inode identity for marker FD matching.
type fileID struct {
	dev uint64
	ino uint64
}

func fileIdent(info fs.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, true
}
