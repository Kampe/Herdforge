//go:build darwin || linux

package harvest

import (
	"fmt"
	"os"
	"syscall"
)

type runtimeFileMeta struct {
	Owner  uint64
	Links  uint64
	Blocks int64
	ID     string
}

func runtimeFileMetaFromInfo(info os.FileInfo) (runtimeFileMeta, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return runtimeFileMeta{}, false
	}
	return runtimeFileMeta{Owner: uint64(st.Uid), Links: uint64(st.Nlink), Blocks: int64(st.Blocks), ID: fmt.Sprintf("%d:%d", st.Dev, st.Ino)}, true
}

func runtimeCurrentUID() (uint64, bool) {
	uid := os.Getuid()
	if uid < 0 {
		return 0, false
	}
	return uint64(uid), true
}

func runtimeInstallSupported() bool { return true }
