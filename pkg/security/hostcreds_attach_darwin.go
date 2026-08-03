//go:build darwin

package security

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// proveAttachDenied requires that the worker cannot task_for_pid / ptrace the broker.
func proveAttachDenied(brokerPID, brokerUID int) error {
	if brokerPID <= 0 {
		return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_invalid"}
	}
	// Confirm process exists and is not us.
	if brokerPID == os.Getpid() {
		return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_is_self"}
	}
	// task_for_pid is not available to unprivileged Go easily; use ptrace attach (PT_ATTACHEXC).
	// On modern macOS this typically fails with EPERM for other users' processes.
	const ptAttach = 10 // PT_ATTACH
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, uintptr(ptAttach), uintptr(brokerPID), 0, 0, 0, 0)
	if errno == 0 {
		// Detach immediately if attach unexpectedly succeeded — boundary broken.
		const ptDetach = 11
		_, _, _ = syscall.Syscall6(syscall.SYS_PTRACE, uintptr(ptDetach), uintptr(brokerPID), 0, 0, 0, 0)
		return &BlockedError{Reason: BlockSecretExposure, Code: "attach_succeeded"}
	}
	// EPERM/EACCES expected.
	if errno != syscall.EPERM && errno != syscall.EACCES {
		// ESRCH: process gone — fail closed.
		if errno == syscall.ESRCH {
			return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_gone"}
		}
		// Other errors: still treat as non-success for attach (denied-ish), but require EPERM ideally.
		// Fail closed if we cannot classify.
		_ = unsafe.Sizeof(errno)
		return &BlockedError{Reason: BlockSecretExposure, Code: fmt.Sprintf("attach_errno:%d", errno)}
	}
	_ = brokerUID
	return nil
}
