//go:build darwin || linux || freebsd || netbsd || openbsd

package resources

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type OSPhysicalMeasurer struct{}

func (m OSPhysicalMeasurer) Measure(path string, maxEntries int) (PhysicalUsage, error) {
	return m.MeasureContext(context.Background(), path, maxEntries)
}

// MeasureContext walks path physically and inspects cancellation at every
// visited entry, so the sweep's deadline reaches inside the walk instead of
// only between walks. One pathological directory therefore cannot monopolize
// the governor's cleanup pass.
//
// A cancelled or expired walk stops visiting immediately and returns the
// partial usage marked Truncated together with the context error. That pair is
// deliberately conservative: every governor call site keys off the error first
// and treats the result as unknown, so a partial figure can never be read as a
// completed measurement or become reclaimed bytes.
func (OSPhysicalMeasurer) MeasureContext(ctx context.Context, path string, maxEntries int) (PhysicalUsage, error) {
	if maxEntries <= 0 {
		return PhysicalUsage{}, errors.New("physical-byte scan requires a positive entry bound")
	}
	if err := ctx.Err(); err != nil {
		return PhysicalUsage{Truncated: true}, err
	}
	if _, err := os.Lstat(path); err != nil {
		return PhysicalUsage{}, err
	}
	var usage PhysicalUsage
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		// Checked before the entry is accounted, so the entry that observes
		// cancellation is not counted and no later entry is visited at all.
		if cancelErr := ctx.Err(); cancelErr != nil {
			usage.Truncated = true
			return cancelErr
		}
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
