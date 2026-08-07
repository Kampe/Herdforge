//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package verifier

import (
	"os"
	"syscall"
	"time"
)

type fac198FileInfo struct {
	mode os.FileMode
	size int64
}

func newFAC198SymlinkInfo() os.FileInfo           { return fac198FileInfo{mode: os.ModeSymlink | 0o777} }
func fac198OwnerFixtureSupported() bool           { return true }
func (f fac198FileInfo) Name() string             { return "link" }
func (f fac198FileInfo) Size() int64              { return f.size }
func (f fac198FileInfo) Mode() os.FileMode        { return f.mode }
func (f fac198FileInfo) ModTime() time.Time       { return time.Time{} }
func (f fac198FileInfo) IsDir() bool              { return f.mode.IsDir() }
func (f fac198FileInfo) Sys() any                 { return &syscall.Stat_t{Uid: 0, Gid: 0} }
func newFAC198DirectoryInfo() os.FileInfo         { return fac198FileInfo{mode: os.ModeDir | 0o755} }
func newFAC198RegularInfo(size int64) os.FileInfo { return fac198FileInfo{mode: 0o644, size: size} }
