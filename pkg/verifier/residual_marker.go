package verifier

import (
	"fmt"
	"os"
)

// createOwnershipMarker creates a private, mode-0600 file used as an
// unforgeable inherited lineage marker. The open file is passed to the
// ownership wrapper as ExtraFiles FD5; descendants that retain the FD are
// causally owned. The path is random under the process temp dir — unrelated
// processes do not open it.
//
// Caller owns the returned *os.File and must Close+Remove it (Close on
// ownedSubprocess does this).
func createOwnershipMarker() (*os.File, string, error) {
	f, err := os.CreateTemp("", "herd-own-*")
	if err != nil {
		return nil, "", fmt.Errorf("create ownership marker: %w", err)
	}
	// Best-effort private mode (CreateTemp is already 0600 on Unix).
	_ = f.Chmod(0o600)
	// Single byte so the inode is a regular file lsof/proc can name.
	if _, err := f.Write([]byte{0}); err != nil {
		path := f.Name()
		_ = f.Close()
		_ = os.Remove(path)
		return nil, "", fmt.Errorf("write ownership marker: %w", err)
	}
	return f, f.Name(), nil
}
