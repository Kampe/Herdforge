//go:build linux

package verifier

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// applyOwnershipContainment puts the ownership supervisor into a new user+PID
// namespace when the kernel allows it. The supervisor becomes PID 1 inside the
// namespace; when it exits, the kernel kills every remaining process in that
// namespace — including setsid/double-fork descendants that left the original
// process group. Namespace setup is required containment; exec.Start fails
// closed if the kernel refuses it, and path residual ownership is not a
// substitute for a missing namespace boundary.
//
// Nested unprivileged userns is skipped when HERD_HERMETIC_CONTAINER=1: the
// outer Docker profile is the isolation boundary, and nested clone(NEWUSER)
// remains unreliable under cap-drop ALL even with seccomp=unconfined on some
// engines. Residual marker lineage is the escaped-writer kill authority.
func applyOwnershipContainment(attr *syscall.SysProcAttr) {
	if attr == nil {
		return
	}
	if os.Getenv(hermeticContainerEnv) == "1" {
		return
	}
	uid := os.Getuid()
	gid := os.Getgid()
	attr.Cloneflags |= unix.CLONE_NEWUSER | unix.CLONE_NEWPID
	attr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: uid, Size: 1},
	}
	attr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: gid, Size: 1},
	}
	// Required for unprivileged user namespaces on many kernels.
	attr.GidMappingsEnableSetgroups = false
}
