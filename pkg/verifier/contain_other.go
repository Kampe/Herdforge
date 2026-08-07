//go:build !linux

package verifier

import "syscall"

// applyOwnershipContainment is a no-op outside Linux. Darwin has no PID
// namespace / subreaper equivalent; escaped-descendant residual ownership uses
// the inherited marker FD (see processesHoldingMarker) plus live-group drain.
func applyOwnershipContainment(attr *syscall.SysProcAttr) {}
