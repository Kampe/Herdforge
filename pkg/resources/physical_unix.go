//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type OSPhysicalMeasurer struct{}

func (OSPhysicalMeasurer) Measure(path string, maxEntries int) (PhysicalUsage, error) {
	if maxEntries <= 0 {
		return PhysicalUsage{}, errors.New("physical-byte scan requires a positive entry bound")
	}
	if _, err := os.Lstat(path); err != nil {
		return PhysicalUsage{}, err
	}
	var usage PhysicalUsage
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		usage.Entries++
		if usage.Entries > maxEntries {
			usage.Truncated = true
			return fs.SkipAll
		}
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil {
			return err
		}
		if stat.Blocks < 0 || uint64(stat.Blocks) > math.MaxUint64/512 || usage.Bytes > math.MaxUint64-uint64(stat.Blocks)*512 {
			return errors.New("physical-byte allocation overflow")
		}
		usage.Bytes += uint64(stat.Blocks) * 512
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return nil
	})
	return usage, err
}
