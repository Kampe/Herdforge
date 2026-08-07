//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package verifier

import (
	"os"
	"syscall"
)

func filesystemOwner(info os.FileInfo) (uid, gid uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Uid), uint64(stat.Gid), true
}
