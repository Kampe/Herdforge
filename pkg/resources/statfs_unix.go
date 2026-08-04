//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// OSBackend is the read-only production backend. It performs one statfs call
// and does not open, write, remove, or otherwise mutate the path.
type OSBackend struct{}

func (OSBackend) StatFS(path string) (Capacity, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return Capacity{}, err
	}
	if stat.Bsize == 0 {
		return Capacity{}, fmt.Errorf("invalid filesystem block size")
	}
	blockSize := uint64(stat.Bsize)
	fsid := fmt.Sprintf("%d:%d", stat.Fsid.Val[0], stat.Fsid.Val[1])
	return capacityFromStatfs(fsid, fmt.Sprintf("%d", stat.Type), stat.Blocks, stat.Bavail, blockSize, stat.Files, stat.Ffree)
}
