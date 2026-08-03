//go:build darwin && cgo

package verifier

/*
#cgo LDFLAGS: -lproc

#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/proc_info.h>
#include <sys/stat.h>
#include <time.h>

static int marker_deadline_expired(struct timespec start, int64_t budget_ns) {
	struct timespec now;
	if (clock_gettime(CLOCK_MONOTONIC, &now) != 0) {
		return 1;
	}
	int64_t elapsed = (int64_t)(now.tv_sec - start.tv_sec) * 1000000000LL +
		(int64_t)(now.tv_nsec - start.tv_nsec);
	return elapsed >= budget_ns;
}

struct marker_holder {
	int pid;
	int64_t start_sec;
	int64_t start_usec;
};

// marker_holders returns identity-bound processes with a vnode FD referencing
// the exact marker inode. PID/start time is sampled before FD inspection and
// rechecked after a match, so PID reuse cannot bind a different incarnation.
// Individual processes and FDs may disappear between libproc calls; those
// races are skipped and the caller's flock loop proves the final fixed point.
// Allocation/system-wide failures are returned fail-closed.
static int marker_holders(const char *path, int64_t budget_ns,
	struct marker_holder **out_holders, int *out_count) {
	*out_holders = NULL;
	*out_count = 0;
	if (budget_ns <= 0) {
		return ETIMEDOUT;
	}

	struct stat want;
	if (stat(path, &want) != 0) {
		return errno;
	}
	struct timespec started;
	if (clock_gettime(CLOCK_MONOTONIC, &started) != 0) {
		return errno;
	}

	int pid_count = proc_listallpids(NULL, 0);
	if (pid_count <= 0) {
		return errno != 0 ? errno : EIO;
	}
	int pid_capacity = pid_count + 256;
	pid_t *pids = calloc((size_t)pid_capacity, sizeof(pid_t));
	struct marker_holder *holders = calloc((size_t)pid_capacity, sizeof(struct marker_holder));
	if (pids == NULL || holders == NULL) {
		free(pids);
		free(holders);
		return ENOMEM;
	}
	pid_count = proc_listallpids(pids, pid_capacity * (int)sizeof(pid_t));
	if (pid_count < 0) {
		int saved = errno != 0 ? errno : EIO;
		free(pids);
		free(holders);
		return saved;
	}

	struct proc_fdinfo *fds = NULL;
	int fd_capacity = 0;
	int holder_count = 0;
	for (int i = 0; i < pid_count; i++) {
		if (marker_deadline_expired(started, budget_ns)) {
			free(fds);
			free(pids);
			free(holders);
			return ETIMEDOUT;
		}
		int pid = pids[i];
		if (pid <= 1) {
			continue;
		}
		struct proc_bsdinfo identity_before;
		int identity_bytes = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0,
			&identity_before, (int)sizeof(identity_before));
		if (identity_bytes != (int)sizeof(identity_before) ||
			identity_before.pbi_pid != (uint32_t)pid) {
			continue;
		}
		int needed = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
		if (needed <= 0) {
			continue;
		}
		int capacity = needed + 64 * (int)sizeof(struct proc_fdinfo);
		if (capacity > fd_capacity) {
			void *grown = realloc(fds, (size_t)capacity);
			if (grown == NULL) {
				free(fds);
				free(pids);
				free(holders);
				return ENOMEM;
			}
			fds = grown;
			fd_capacity = capacity;
		}
		int bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, fd_capacity);
		if (bytes <= 0) {
			continue;
		}
		int fd_count = bytes / (int)sizeof(struct proc_fdinfo);
		for (int j = 0; j < fd_count; j++) {
			if (marker_deadline_expired(started, budget_ns)) {
				free(fds);
				free(pids);
				free(holders);
				return ETIMEDOUT;
			}
			if (fds[j].proc_fdtype != PROX_FDTYPE_VNODE) {
				continue;
			}
			struct vnode_fdinfowithpath vnode;
			int vnode_bytes = proc_pidfdinfo(pid, fds[j].proc_fd,
				PROC_PIDFDVNODEPATHINFO, &vnode, (int)sizeof(vnode));
			if (vnode_bytes != (int)sizeof(vnode)) {
				continue;
			}
			if (vnode.pvip.vip_vi.vi_stat.vst_dev == want.st_dev &&
				vnode.pvip.vip_vi.vi_stat.vst_ino == want.st_ino) {
				struct proc_bsdinfo identity_after;
				identity_bytes = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0,
					&identity_after, (int)sizeof(identity_after));
				if (identity_bytes == (int)sizeof(identity_after) &&
					identity_after.pbi_pid == identity_before.pbi_pid &&
					identity_after.pbi_start_tvsec == identity_before.pbi_start_tvsec &&
					identity_after.pbi_start_tvusec == identity_before.pbi_start_tvusec) {
					holders[holder_count].pid = pid;
					holders[holder_count].start_sec = (int64_t)identity_before.pbi_start_tvsec;
					holders[holder_count].start_usec = (int64_t)identity_before.pbi_start_tvusec;
					holder_count++;
				}
				break;
			}
		}
	}

	free(fds);
	free(pids);
	if (holder_count == 0) {
		free(holders);
		return 0;
	}
	*out_holders = holders;
	*out_count = holder_count;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// processesHoldingMarkerUntil uses libproc to inspect vnode FDs by device and
// inode. This avoids spawning lsof for every fixed-point pass while preserving
// exact marker identity, PID/start-time token binding, and the caller's bound.
func processesHoldingMarkerUntil(markerPath string, deadline time.Time) ([]procToken, error) {
	if markerPath == "" {
		return nil, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, fmt.Errorf("processesHoldingMarker: deadline exceeded")
	}
	cPath := C.CString(markerPath)
	defer C.free(unsafe.Pointer(cPath))

	var holders *C.struct_marker_holder
	var holderCount C.int
	code := C.marker_holders(cPath, C.int64_t(remaining.Nanoseconds()), &holders, &holderCount)
	if code != 0 {
		return nil, fmt.Errorf("processesHoldingMarker libproc: %w", syscall.Errno(code))
	}
	if holders == nil || holderCount == 0 {
		return nil, nil
	}
	defer C.free(unsafe.Pointer(holders))

	excl := residualExcludePIDs()
	native := unsafe.Slice(holders, int(holderCount))
	out := make([]procToken, 0, len(native))
	seen := make(map[int]struct{}, len(native))
	for _, holder := range native {
		pid := int(holder.pid)
		if pid <= 1 {
			continue
		}
		if _, skip := excl[pid]; skip {
			continue
		}
		tok := procToken{pid: pid, startSec: int64(holder.start_sec), startUsec: int64(holder.start_usec)}
		if _, ok := seen[tok.pid]; ok {
			continue
		}
		seen[tok.pid] = struct{}{}
		out = append(out, tok)
	}
	return out, nil
}
