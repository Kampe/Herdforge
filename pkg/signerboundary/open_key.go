//go:build unix

package signerboundary

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openKeyVerified opens path with O_RDONLY|O_CLOEXEC|O_NOFOLLOW then re-checks
// owner/mode/nlink/dev+ino via Fstat to close audit→open TOCTOU (FAC-169).
func openKeyVerified(path string, wantUID int) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = f.Close()
		return nil, fmt.Errorf("%w: not a regular file after open", ErrKeyExposed)
	}
	if int(st.Uid) != wantUID {
		_ = f.Close()
		return nil, fmt.Errorf("%w: fstat uid %d want %d", ErrKeyExposed, st.Uid, wantUID)
	}
	if st.Mode&0o077 != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: fstat mode still group/world accessible", ErrKeyExposed)
	}
	if st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: fstat nlink=%d", ErrKeyExposed, st.Nlink)
	}
	// Recheck path identity with Lstat (syscall.Stat_t) for dev+ino TOCTOU.
	fi, err := os.Lstat(path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	lst, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("%w: cannot recheck inode", ErrProvisioning)
	}
	if uint64(lst.Dev) != uint64(st.Dev) || uint64(lst.Ino) != uint64(st.Ino) {
		_ = f.Close()
		return nil, fmt.Errorf("%w: path replaced between open and recheck (TOCTOU)", ErrKeyExposed)
	}
	return f, nil
}
