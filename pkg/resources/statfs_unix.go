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
	if stat.Bsize <= 0 {
		return Capacity{}, fmt.Errorf("invalid filesystem block size")
	}
	blockSize := uint64(stat.Bsize)
	fsid := fmt.Sprintf("%d:%d", stat.Fsid.Val[0], stat.Fsid.Val[1])

	toUint64 := func(v int64) uint64 {
		if v < 0 {
			return 0
		}
		return uint64(v)
	}

	return capacityFromStatfs(
		fsid,
		fmt.Sprintf("%d", stat.Type),
		toUint64(int64(stat.Blocks)),
		toUint64(int64(stat.Bavail)),
		blockSize,
		toUint64(int64(stat.Files)),
		toUint64(int64(stat.Ffree)),
	)
}
