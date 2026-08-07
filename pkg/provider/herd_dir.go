package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// secureHerd holds an open directory fd for worktree/.herd so all subsequent
// openat/renameat/unlinkat operations bind to the validated inode and cannot
// race a parent rename→symlink swap.
type secureHerd struct {
	path string // absolute path for diagnostics only
	fd   int
}

func (h *secureHerd) Close() error {
	if h == nil || h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}

// openSecureHerd opens (creating if needed) worktree/.herd via openat with
// O_NOFOLLOW on both the worktree final component and .herd. Does NOT
// EvalSymlinks-then-pathname-open (that TOCTOU lets a concurrent swap of the
// worktree path redirect the fd). The returned handle must be Closed.
func openSecureHerd(worktreePath string) (*secureHerd, error) {
	if worktreePath == "" {
		return nil, fmt.Errorf("provider: empty worktree path")
	}
	absWT, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, err
	}
	// Open the worktree directory as the final path component with O_NOFOLLOW.
	// Intermediate system components (e.g. /tmp→/private/tmp) may resolve;
	// a concurrent swap of the final leaf to a symlink fails closed.
	wtFd, err := unix.Open(absWT, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ELOOP || err == unix.ENOTDIR {
			return nil, fmt.Errorf("provider: refuse worktree path symlink/non-dir: %w", err)
		}
		// Fallback: some platforms reject O_NOFOLLOW on non-symlink dirs with EINVAL.
		if err == unix.EINVAL {
			wtFd, err = unix.Open(absWT, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("provider: open worktree dir: %w", err)
		}
		// Post-open identity: must be a directory; refuse if path is now a symlink leaf.
		if fi, lerr := os.Lstat(absWT); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			_ = unix.Close(wtFd)
			return nil, fmt.Errorf("provider: refuse worktree path that is a symlink: %s", absWT)
		}
	}
	// fstat: ensure we hold a real directory inode (not a weird fd).
	var st unix.Stat_t
	if err := unix.Fstat(wtFd, &st); err != nil {
		_ = unix.Close(wtFd)
		return nil, fmt.Errorf("provider: fstat worktree: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(wtFd)
		return nil, fmt.Errorf("provider: worktree fd is not a directory")
	}
	defer unix.Close(wtFd)

	if err := unix.Mkdirat(wtFd, ".herd", 0o755); err != nil && err != unix.EEXIST {
		return nil, fmt.Errorf("provider: mkdirat .herd: %w", err)
	}
	herdFd, err := unix.Openat(wtFd, ".herd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ELOOP || err == unix.ENOTDIR {
			return nil, fmt.Errorf("provider: refuse .herd directory symlink: %w", err)
		}
		return nil, fmt.Errorf("provider: openat .herd: %w", err)
	}
	if err := unix.Fsync(wtFd); err != nil {
		_ = unix.Close(herdFd)
		return nil, fmt.Errorf("provider: fsync worktree after .herd create: %w", err)
	}
	return &secureHerd{path: filepath.Join(absWT, ".herd"), fd: herdFd}, nil
}

func (h *secureHerd) atomicWrite(name string, raw []byte, mode os.FileMode) error {
	if h == nil || h.fd < 0 {
		return fmt.Errorf("provider: secureHerd closed")
	}
	if fd, err := unix.Openat(h.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
		_ = unix.Close(fd)
	} else if err == unix.ELOOP {
		return fmt.Errorf("provider: refuse write through symlink %s", name)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", name, time.Now().UnixNano())
	fd, err := unix.Openat(h.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("provider: openat tmp %s: %w", tmp, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = unix.Unlinkat(h.fd, tmp, 0)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = unix.Unlinkat(h.fd, tmp, 0)
		return err
	}
	if err := f.Close(); err != nil {
		_ = unix.Unlinkat(h.fd, tmp, 0)
		return err
	}
	if err := unix.Renameat(h.fd, tmp, h.fd, name); err != nil {
		_ = unix.Unlinkat(h.fd, tmp, 0)
		return fmt.Errorf("provider: renameat %s: %w", name, err)
	}
	if err := unix.Fsync(h.fd); err != nil {
		return fmt.Errorf("provider: fsync parent after write %s: %w", name, err)
	}
	return nil
}

func (h *secureHerd) read(name string) ([]byte, error) {
	if h == nil || h.fd < 0 {
		return nil, fmt.Errorf("provider: secureHerd closed")
	}
	fd, err := unix.Openat(h.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, os.ErrNotExist
		}
		if err == unix.ELOOP {
			return nil, fmt.Errorf("provider: refuse read through symlink %s", name)
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("provider: refuse non-regular %s", name)
	}
	buf := make([]byte, st.Size())
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

func (h *secureHerd) remove(name string) error {
	if h == nil || h.fd < 0 {
		return fmt.Errorf("provider: secureHerd closed")
	}
	err := unix.Unlinkat(h.fd, name, 0)
	if err == nil || err == unix.ENOENT {
		return nil
	}
	return err
}

func (h *secureHerd) fsync() error {
	if h == nil || h.fd < 0 {
		return fmt.Errorf("provider: secureHerd closed")
	}
	return unix.Fsync(h.fd)
}
