//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package verifier

import "os"

func filesystemOwner(os.FileInfo) (uid, gid uint64, ok bool) {
	return 0, 0, false
}
