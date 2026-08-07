package verifier

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// markerLineageDrainedFn is a narrow mutation seam for proving that the
// inherited marker lock is load-bearing. Production always uses the kernel
// lock probe below.
var markerLineageDrainedFn = markerLineageDrained

// markerLineageScanFn and markerTokenNoteFn are narrow seams for proving
// marker-holder retry and exact-token ownership without live writer fixtures.
var markerLineageScanFn = processesHoldingMarkerUntil
var markerTokenNoteFn = (*ownedSubprocess).noteCausal
var markerTokenLiveFn = procToken.isLiveTarget
var markerLeaderLiveFn = func(h ownedHandle) bool { return h.tok.stillSame() }

// createOwnershipMarker creates a private, mode-0600 file used as an inherited
// locked lineage marker. The open file is passed to the ownership wrapper as
// ExtraFiles FD5; descendants that retain the FD are causally owned. The path
// is random under the process temp dir and is never candidate-path authority.
//
// Caller owns the returned *os.File and must Close+Remove it (Close on
// ownedSubprocess does this).
func createOwnershipMarker() (*os.File, string, error) {
	f, err := os.CreateTemp("", "herd-own-*")
	if err != nil {
		return nil, "", fmt.Errorf("create ownership marker: %w", err)
	}
	path := f.Name()
	cleanup := func(cause error) (*os.File, string, error) {
		return nil, "", errors.Join(cause, f.Close(), removeMarkerPath(path))
	}
	if err := f.Chmod(0o600); err != nil {
		return cleanup(fmt.Errorf("chmod ownership marker: %w", err))
	}
	// Single byte so the inode is a regular file lsof/proc can name.
	if _, err := f.Write([]byte{0}); err != nil {
		return cleanup(fmt.Errorf("write ownership marker: %w", err))
	}
	// flock is attached to the inherited open-file description. Once the
	// verifier and supervisor close their copies, a separate probe can acquire
	// this lock if and only if the last marked descendant has closed/exited.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return cleanup(fmt.Errorf("lock ownership marker: %w", err))
	}
	return f, f.Name(), nil
}

// markerLineageDrained is the kernel fixed-point proof for marker ownership.
// A successful lock on a separately opened file description proves no process
// retains the inherited locked marker FD. EWOULDBLOCK means at least one
// marked holder remains; time and candidate-path contact are not authority.
func markerLineageDrained(markerPath string) (bool, error) {
	if markerPath == "" {
		return false, errors.New("marker lineage drained: empty marker path")
	}
	probe, err := os.OpenFile(markerPath, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("marker lineage drained open: %w", err)
	}
	lockErr := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
		if closeErr := probe.Close(); closeErr != nil {
			return false, fmt.Errorf("marker lineage drained close contended probe: %w", closeErr)
		}
		return false, nil
	}
	if lockErr != nil {
		closeErr := probe.Close()
		return false, errors.Join(fmt.Errorf("marker lineage drained lock: %w", lockErr), closeErr)
	}
	unlockErr := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
	closeErr := probe.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return false, fmt.Errorf("marker lineage drained release probe: %w", err)
	}
	return true, nil
}

func removeMarkerPath(markerPath string) error {
	if markerPath == "" {
		return nil
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove ownership marker: %w", err)
	}
	return nil
}

// processesHoldingMarker keeps direct tests concise while production passes
// its fixed-point deadline into the platform scanner.
func processesHoldingMarker(markerPath string) ([]procToken, error) {
	return processesHoldingMarkerUntil(markerPath, time.Now().Add(processGroupGoneBound))
}
