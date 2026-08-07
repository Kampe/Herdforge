package signerboundary

import (
	"fmt"
	"os"
	"path/filepath"
)

// auditKeyMaterialPath enforces hardlink/symlink-safe key storage for the key
// node and its immediate parent (the key dir we control):
//   - key path must not itself be a symlink (Lstat)
//   - parent directory must not be a symlink
//   - regular file only, nlink == 1
//   - owner == wantUID, mode 0600
//   - parent not group/world-writable
//
// System path components (e.g. macOS /var → /private/var) are not rejected:
// that is ambient OS layout, not a worker-controlled rename/symlink escape.
// Escape risk is at the key node and its parent under the key store.
//
// Pre-opened FD inheritance is mitigated by O_NOFOLLOW|O_CLOEXEC open in the server.
func auditKeyMaterialPath(keyPath string, wantUID int) error {
	abs, err := filepath.Abs(keyPath)
	if err != nil {
		return err
	}

	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("%w: key path: %v", ErrProvisioning, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: key is a symlink", ErrKeyExposed)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: key is not a regular file", ErrKeyExposed)
	}
	if nlink, ok := statNlink(fi); ok && nlink != 1 {
		return fmt.Errorf("%w: key has nlink=%d (hardlink alias risk)", ErrKeyExposed, nlink)
	}
	uid, ok := statUID(fi)
	if !ok {
		return fmt.Errorf("%w: cannot read key owner uid", ErrProvisioning)
	}
	if uid != wantUID {
		return fmt.Errorf("%w: key owner uid %d want %d", ErrProvisioning, uid, wantUID)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: key mode %v", ErrKeyExposed, fi.Mode().Perm())
	}

	parent := filepath.Dir(abs)
	pfi, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if pfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: key parent is symlink", ErrKeyExposed)
	}
	if !pfi.IsDir() {
		return fmt.Errorf("%w: key parent is not a directory", ErrKeyExposed)
	}
	if pfi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: key parent is group/world-writable (%v)", ErrKeyExposed, pfi.Mode().Perm())
	}
	return nil
}

// openKeyNoFollow is retained for tests; production uses openKeyVerified.
func openKeyNoFollow(path string) (*os.File, error) {
	return openKeyVerified(path, os.Getuid())
}
