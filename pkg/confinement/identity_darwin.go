//go:build darwin

package confinement

import (
	"os"
	"syscall"
)

func platformIdentity(info os.FileInfo) (fileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev <= 0 || stat.Ino <= 0 {
		return fileIdentity{}, ErrUnsupportedIdentity
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func platformNlink(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink < 0 {
		return 0
	}
	return uint64(stat.Nlink)
}
