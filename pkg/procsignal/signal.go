// Package procsignal fail-closes host-wide and sentinel signal targets (FAC-174).
//
// The FAC-172 incidents called kill(-1, SIGTERM) from a non-root login user and
// terminated ambient macOS GUI processes. Unix treats pid -1 as "every process
// the caller may signal" and pid/pgid 0/1 as special session/init targets.
//
// Invariants:
//   - The only path to syscall.Kill is the unexported hostBackend.
//   - hostBackend re-validates the raw kill(2) argument (including -1).
//   - Production cancels use OwnedGroup handles claimed from a live *os.Process.
//   - Destructive real-signal profiles require hermetic opt-in AND isolation proof.
//   - Unit tests use FakeBackend; they cannot signal a real process.
package procsignal

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var (
	// ErrUnsafeTarget is returned when a PID/PGID would expand into a host-wide
	// or caller/session signal before any syscall is issued.
	ErrUnsafeTarget = errors.New("procsignal: unsafe signal target refused")

	// ErrHermeticRequired is returned when a destructive real-signal profile
	// runs without the hermetic opt-in environment receipt.
	ErrHermeticRequired = errors.New("procsignal: destructive profile requires hermetic opt-in")

	// ErrIsolationRequired is returned when a destructive profile lacks
	// namespace/VM/fixture-UID isolation proof before a real signal.
	ErrIsolationRequired = errors.New("procsignal: destructive profile requires isolation proof")

	// ErrBackendRequired is returned when a kill is requested with a nil backend.
	ErrBackendRequired = errors.New("procsignal: signal backend is required")

	// ErrNotOwned is returned when an OwnedGroup token is missing or mismatched.
	ErrNotOwned = errors.New("procsignal: process group is not claimed as owned")
)

// HermeticEnv is the explicit opt-in for destructive real-signal proofs.
// Production cancel paths never set this; only a hermetic runner may.
const HermeticEnv = "HERD_PROCSIGNAL_HERMETIC"

// IsolationEnv selects the isolation mode proven by the hermetic runner.
// Accepted values: "pidns", "vm", "docker", "fixture-uid".
const IsolationEnv = "HERD_PROCSIGNAL_ISOLATION"

// IsolationIDEnv carries the opaque namespace/VM/container identity from the
// hermetic runner (required for pidns|vm|docker modes).
const IsolationIDEnv = "HERD_PROCSIGNAL_NS_ID"

// FixtureUIDEnv is the expected UID string for isolation mode "fixture-uid".
// The running process UID must match exactly.
const FixtureUIDEnv = "HERD_PROCSIGNAL_FIXTURE_UID"

// Backend is the injectable kill seam for tests (FakeBackend only).
// Production code cannot inject a raw host kill: hostBackend is unexported.
type Backend interface {
	Kill(pid int, sig syscall.Signal) error
}

// FakeBackend records kill attempts and never issues a real signal. Use it for
// all unit / incident-class tests. It deliberately does NOT validate targets so
// mutation tests can show what an unguarded kill argument looks like.
type FakeBackend struct {
	Calls []KillCall
	Err   error
}

// KillCall is one recorded Kill attempt.
type KillCall struct {
	PID int
	Sig syscall.Signal
}

// Kill implements Backend.
func (f *FakeBackend) Kill(pid int, sig syscall.Signal) error {
	if f == nil {
		return ErrBackendRequired
	}
	f.Calls = append(f.Calls, KillCall{PID: pid, Sig: sig})
	return f.Err
}

// callerPID / callerPgid are seams so mutation tests can bind identity without
// forking. Production defaults resolve the live process.
var (
	callerPID  = os.Getpid
	callerPgid = syscall.Getpgrp
)

// ValidatePID rejects host-wide, sentinel, and self PIDs before any signal
// syscall. Valid PIDs are strictly greater than 1 and not the calling process.
// Negative values are never valid individual PIDs (use ValidatePGID / process
// groups instead). math.MinInt and all negatives fail the <=1 or <0 checks.
func ValidatePID(pid int) error {
	if pid <= 1 {
		defaultMeter.noteAmbientRefused()
		return fmt.Errorf("%w: pid %d is a host-wide or sentinel target", ErrUnsafeTarget, pid)
	}
	if pid == callerPID() {
		return fmt.Errorf("%w: pid %d is the calling process", ErrUnsafeTarget, pid)
	}
	return nil
}

// ValidatePGID rejects process-group IDs that expand into host-wide or
// caller-session signals. kill(-1, sig) is the FAC-172 class: Unix broadcasts
// to every same-UID process the caller may signal. PGID 0 means "current
// group"; PGID 1 is init/session-class. The caller's own process group is
// also refused — a miscomputed cancel must not SIGKILL the parent shell tree.
func ValidatePGID(pgid int) error {
	if pgid <= 1 {
		defaultMeter.noteAmbientRefused()
		return fmt.Errorf("%w: process group %d is a host-wide or sentinel target", ErrUnsafeTarget, pgid)
	}
	if pgid == callerPgid() {
		return fmt.Errorf("%w: process group %d is the caller's process group", ErrUnsafeTarget, pgid)
	}
	return nil
}

// validateKillArg re-checks the raw kill(2) pid argument at the host boundary.
// This is the last line of defense if a future caller bypasses ValidatePID/PGID.
// pid may be negative (POSIX process-group form).
func validateKillArg(pid int) error {
	switch {
	case pid == -1, pid == 0, pid == 1:
		defaultMeter.noteAmbientRefused()
		return fmt.Errorf("%w: kill arg %d is a host-wide or sentinel target", ErrUnsafeTarget, pid)
	case pid < -1:
		return ValidatePGID(-pid)
	default: // pid > 1
		return ValidatePID(pid)
	}
}

// KillProcess signals one process after ValidatePID via a test FakeBackend.
// Production real signals must use SignalExactProcess or OwnedGroup helpers;
// passing anything other than FakeBackend that reaches the host is impossible
// because hostBackend is unexported — only package helpers construct it.
func KillProcess(backend Backend, pid int, sig syscall.Signal) error {
	if backend == nil {
		return ErrBackendRequired
	}
	if err := ValidatePID(pid); err != nil {
		return err
	}
	return backend.Kill(pid, sig)
}

// KillProcessGroup signals a process group after ValidatePGID via FakeBackend
// (tests) or package-internal helpers. The kill argument is the POSIX
// negative-pgid form (-pgid).
func KillProcessGroup(backend Backend, pgid int, sig syscall.Signal) error {
	if backend == nil {
		return ErrBackendRequired
	}
	if err := ValidatePGID(pgid); err != nil {
		return err
	}
	return backend.Kill(-pgid, sig)
}

// hostBackend is the ONLY type that may call syscall.Kill. It is unexported so
// external packages cannot "use procsignal" while still issuing raw host-wide
// kills. mode controls hermetic policy for destructive profiles.
type hostBackend struct {
	// destructive requires hermetic + isolation before the syscall.
	// owned production cancels set destructive=false.
	destructive bool
}

func (h hostBackend) Kill(pid int, sig syscall.Signal) error {
	if err := validateKillArg(pid); err != nil {
		return err
	}
	if h.destructive {
		if err := RequireHermetic(); err != nil {
			return err
		}
		if err := RequireIsolation(); err != nil {
			return err
		}
	}
	defaultMeter.noteHostKill()
	return syscall.Kill(pid, sig)
}

// RequireHermetic fails closed when a destructive profile runs without the
// hermetic opt-in environment receipt.
func RequireHermetic() error {
	if os.Getenv(HermeticEnv) != "1" {
		return ErrHermeticRequired
	}
	return nil
}

// RequireIsolation fails closed unless the hermetic runner has published a
// recognized isolation mode and identity (PID namespace, VM, docker, or
// fixture UID). Must be called only after RequireHermetic succeeds.
func RequireIsolation() error {
	if err := RequireHermetic(); err != nil {
		return err
	}
	mode := os.Getenv(IsolationEnv)
	switch mode {
	case "pidns", "vm", "docker":
		if os.Getenv(IsolationIDEnv) == "" {
			return fmt.Errorf("%w: %s mode requires %s", ErrIsolationRequired, mode, IsolationIDEnv)
		}
		return nil
	case "fixture-uid":
		want := os.Getenv(FixtureUIDEnv)
		if want == "" {
			return fmt.Errorf("%w: fixture-uid mode requires %s", ErrIsolationRequired, FixtureUIDEnv)
		}
		if fmt.Sprintf("%d", os.Getuid()) != want {
			return fmt.Errorf("%w: running uid %d does not match fixture uid %s", ErrIsolationRequired, os.Getuid(), want)
		}
		return nil
	default:
		return fmt.Errorf("%w: set %s to pidns|vm|docker|fixture-uid", ErrIsolationRequired, IsolationEnv)
	}
}

// DestructiveKillProcessGroup is the only exported path for intentional
// real SIGTERM/SIGKILL against a process group from a destructive proof.
// It refuses without hermetic opt-in and isolation proof, then validates
// the target before hostBackend issues the syscall.
func DestructiveKillProcessGroup(pgid int, sig syscall.Signal) error {
	if err := RequireHermetic(); err != nil {
		return err
	}
	if err := RequireIsolation(); err != nil {
		return err
	}
	if err := ValidatePGID(pgid); err != nil {
		return err
	}
	return hostBackend{destructive: true}.Kill(-pgid, sig)
}

// DestructiveKillProcess is the single-process form of the destructive path.
func DestructiveKillProcess(pid int, sig syscall.Signal) error {
	if err := RequireHermetic(); err != nil {
		return err
	}
	if err := RequireIsolation(); err != nil {
		return err
	}
	if err := ValidatePID(pid); err != nil {
		return err
	}
	return hostBackend{destructive: true}.Kill(pid, sig)
}
