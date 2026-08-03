//go:build !linux

package verifier

import "syscall"

// applyOwnershipContainment is a no-op outside Linux. Darwin has no PID
// namespace / subreaper equivalent; residual ownership uses candidate-path
// open-file discovery (see processesTouchingDir) plus live-group drain.
func applyOwnershipContainment(attr *syscall.SysProcAttr) {}
