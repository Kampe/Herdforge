//go:build linux

package security

import (
	"fmt"
	"os"
	"syscall"
)

func proveAttachDenied(brokerPID, brokerUID int) error {
	if brokerPID <= 0 {
		return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_invalid"}
	}
	if brokerPID == os.Getpid() {
		return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_is_self"}
	}
	// PTRACE_ATTACH
	const ptraceAttach = 16
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, ptraceAttach, uintptr(brokerPID), 0, 0, 0, 0)
	if errno == 0 {
		const ptraceDetach = 17
		_, _, _ = syscall.RawSyscall6(syscall.SYS_PTRACE, ptraceDetach, uintptr(brokerPID), 0, 0, 0, 0)
		return &BlockedError{Reason: BlockSecretExposure, Code: "attach_succeeded"}
	}
	if errno != syscall.EPERM && errno != syscall.EACCES {
		if errno == syscall.ESRCH {
			return &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_gone"}
		}
		return &BlockedError{Reason: BlockSecretExposure, Code: fmt.Sprintf("attach_errno:%d", errno)}
	}
	_ = brokerUID
	return nil
}
