package signerboundary

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Permission-class errors we accept as "OS denied access" for key/material reads.
// ENOENT, EINVAL, EISDIR, etc. are NOT denials — they mean the harness failed
// or the fixture is wrong (FAC-169 follow-up §1: unknown exit is BLOCKED).
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return true
	}
	// path errors wrap errno
	var pe *os.PathError
	if errors.As(err, &pe) {
		return isPermissionDenied(pe.Err)
	}
	return false
}

func requirePathExists(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: path must exist for denial probe (got %v) — harness failure not denial", ErrProvisioning, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: path is symlink", ErrKeyExposed)
	}
	return nil
}

// classifyAttachError returns (deniedOK, harnessFailure).
// deniedOK means the OS refused attach in a way consistent with isolation.
// harnessFailure means the probe could not run (tooling/API unavailable).
func classifyAttachError(err error) (deniedOK bool, harnessFailure error) {
	if err == nil {
		return false, nil // attach succeeded — boundary failure handled by caller
	}
	// Only EPERM/EACCES count as isolation denial. ESRCH, EINVAL, ENOSYS,
	// unsupported, and "process gone" are harness failures (BLOCKED).
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("%w: attach target vanished (ESRCH) — not a denial proof", ErrProvisioning)
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		return false, fmt.Errorf("%w: attach unsupported/invalid (%v) — not a denial proof", ErrProvisioning, err)
	}
	return false, fmt.Errorf("%w: attach probe ambiguous error %v (not isolation denial)", ErrProvisioning, err)
}

// observedErrnoString normalizes an error for ProbeReceipt.ObservedErrno.
func observedErrnoString(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, syscall.EPERM) {
		return "EPERM"
	}
	if errors.Is(err, syscall.EACCES) {
		return "EACCES"
	}
	if errors.Is(err, syscall.ESRCH) {
		return "ESRCH"
	}
	return err.Error()
}
