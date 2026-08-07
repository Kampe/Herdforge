//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package verifier

import (
	"os"
	"time"
)

type fac198FileInfo struct{}

func newFAC198SymlinkInfo() os.FileInfo      { return fac198FileInfo{} }
func fac198OwnerFixtureSupported() bool      { return false }
func (fac198FileInfo) Name() string          { return "link" }
func (fac198FileInfo) Size() int64           { return 0 }
func (fac198FileInfo) Mode() os.FileMode     { return os.ModeSymlink | 0o777 }
func (fac198FileInfo) ModTime() time.Time    { return time.Time{} }
func (fac198FileInfo) IsDir() bool           { return false }
func (fac198FileInfo) Sys() any              { return nil }
func newFAC198DirectoryInfo() os.FileInfo    { return fac198FileInfo{} }
func newFAC198RegularInfo(int64) os.FileInfo { return fac198FileInfo{} }
