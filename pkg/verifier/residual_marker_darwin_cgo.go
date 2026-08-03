//go:build darwin && cgo

package verifier

/*
#cgo LDFLAGS: -lproc

#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/stat.h>
#include <time.h>

static int marker_errno_or_eio(int saved_errno) {
	return saved_errno != 0 ? saved_errno : EIO;
}

static int marker_deadline_expired(struct timespec start, int64_t budget_ns,
	int *out_error) {
	struct timespec now;
	errno = 0;
	if (clock_gettime(CLOCK_MONOTONIC, &now) != 0) {
		*out_error = marker_errno_or_eio(errno);
		return 1;
	}
	*out_error = 0;
	int64_t elapsed = (int64_t)(now.tv_sec - start.tv_sec) * 1000000000LL +
		(int64_t)(now.tv_nsec - start.tv_nsec);
	if (elapsed >= budget_ns) {
		*out_error = ETIMEDOUT;
		return 1;
	}
	return 0;
}

struct marker_holder {
	int pid;
	int64_t start_sec;
	int64_t start_usec;
};

enum marker_stage {
	MARKER_STAGE_STAT = 1,
	MARKER_STAGE_CLOCK,
	MARKER_STAGE_PIDLIST,
	MARKER_STAGE_IDENTITY,
	MARKER_STAGE_FDLIST,
	MARKER_STAGE_VNODE,
	MARKER_STAGE_IDENTITY_RECHECK,
	MARKER_STAGE_ALLOC,
	MARKER_STAGE_DEADLINE,
	MARKER_STAGE_OUTPUT,
};

static int marker_vanished(int result, int saved_errno) {
	return result <= 0 && (saved_errno == ESRCH || saved_errno == EIO);
}

static int marker_clock_failure_for_test(int saved_errno) {
	return marker_errno_or_eio(saved_errno);
}

static int marker_deadline_result_for_test(int injected_errno) {
	return marker_errno_or_eio(injected_errno);
}

static int marker_output_decision(int has_holders, int holder_count) {
	return holder_count >= 0 &&
		((holder_count == 0 && !has_holders) || (holder_count > 0 && has_holders));
}

// marker_capacity_decision is shared by the native inventory loops. A
// nonpositive result is never an empty success; a full buffer gets a bounded
// growth attempt, and exhaustion is a hard truncation error.
static int marker_capacity_decision(int result, int capacity, int saved_errno,
	int *attempts) {
	if (saved_errno != 0) {
		return saved_errno;
	}
	if (result <= 0) {
		return EIO;
	}
	if (result < capacity) {
		return 0;
	}
	if (++(*attempts) >= 4) {
		return EOVERFLOW;
	}
	return 1;
}

// marker_identity_decision classifies one exact-size identity observation.
// A vanished process may be skipped; exact bytes with a mismatched identity
// are an inspection failure, never evidence that the process is absent.
static int marker_identity_decision(int result, int expected, int saved_errno,
	int identity_matches) {
	if (result != expected) {
		if (marker_vanished(result, saved_errno)) {
			return 0;
		}
		return saved_errno != 0 ? saved_errno : EIO;
	}
	if (saved_errno != 0) {
		return saved_errno;
	}
	return identity_matches ? 1 : EIO;
}

// marker_holders returns identity-bound processes with a vnode FD referencing
// the exact marker inode. PID/start time is sampled before FD inspection and
// rechecked after a match, so PID reuse cannot bind a different incarnation.
// Individual processes and FDs may disappear between libproc calls. Only the
// documented vanished-process results are skipped; all other inspection and
// inventory failures are returned fail-closed to the caller.
static int marker_holders(const char *path, int64_t budget_ns,
	struct marker_holder **out_holders, int *out_count, int *out_stage) {
	*out_holders = NULL;
	*out_count = 0;
	*out_stage = 0;
	if (budget_ns <= 0) {
		*out_stage = MARKER_STAGE_CLOCK;
		return ETIMEDOUT;
	}

	errno = 0;
	struct stat want;
	if (stat(path, &want) != 0) {
		*out_stage = MARKER_STAGE_STAT;
		return marker_errno_or_eio(errno);
	}
	struct timespec started;
	errno = 0;
	if (clock_gettime(CLOCK_MONOTONIC, &started) != 0) {
		*out_stage = MARKER_STAGE_CLOCK;
		return marker_errno_or_eio(errno);
	}

	errno = 0;
	int pid_count = proc_listallpids(NULL, 0);
	if (pid_count <= 0) {
		*out_stage = MARKER_STAGE_PIDLIST;
		return marker_errno_or_eio(errno);
	}
	int pid_capacity = pid_count + 256;
	pid_t *pids = calloc((size_t)pid_capacity, sizeof(pid_t));
	struct marker_holder *holders = calloc((size_t)pid_capacity, sizeof(struct marker_holder));
	if (pids == NULL || holders == NULL) {
		*out_stage = MARKER_STAGE_ALLOC;
		free(pids);
		free(holders);
		return ENOMEM;
	}
	int pid_attempts = 0;
	for (;;) {
		errno = 0;
		pid_count = proc_listallpids(pids, pid_capacity * (int)sizeof(pid_t));
		int pid_decision = marker_capacity_decision(pid_count, pid_capacity,
			errno, &pid_attempts);
		if (pid_decision != 0 && pid_decision != 1) {
			*out_stage = MARKER_STAGE_PIDLIST;
			free(pids);
			free(holders);
			return pid_decision;
		}
		if (pid_decision == 0) {
			break;
		}
		int next_capacity = pid_capacity * 2;
		pid_t *grown_pids = calloc((size_t)next_capacity, sizeof(pid_t));
		struct marker_holder *grown_holders = calloc((size_t)next_capacity,
			sizeof(struct marker_holder));
		if (grown_pids == NULL || grown_holders == NULL) {
			free(grown_pids);
			free(grown_holders);
			free(pids);
			free(holders);
			*out_stage = MARKER_STAGE_ALLOC;
			return ENOMEM;
		}
		memcpy(grown_pids, pids, (size_t)pid_capacity * sizeof(pid_t));
		memcpy(grown_holders, holders,
			(size_t)pid_capacity * sizeof(struct marker_holder));
		free(pids);
		free(holders);
		pids = grown_pids;
		holders = grown_holders;
		pid_capacity = next_capacity;
	}

	struct proc_fdinfo *fds = NULL;
	int fd_capacity = 0;
	int holder_count = 0;
	for (int i = 0; i < pid_count; i++) {
		int deadline_error = 0;
		if (marker_deadline_expired(started, budget_ns, &deadline_error)) {
			*out_stage = deadline_error == ETIMEDOUT ? MARKER_STAGE_DEADLINE : MARKER_STAGE_CLOCK;
			free(fds);
			free(pids);
			free(holders);
			return deadline_error;
		}
		int pid = pids[i];
		if (pid <= 1) {
			continue;
		}
		struct proc_bsdinfo identity_before;
		errno = 0;
		int identity_bytes = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0,
			&identity_before, (int)sizeof(identity_before));
		int identity_errno = errno;
		int identity_decision = marker_identity_decision(identity_bytes,
			(int)sizeof(identity_before), identity_errno,
			identity_bytes == (int)sizeof(identity_before) &&
			identity_before.pbi_pid == (uint32_t)pid);
		if (identity_decision == 0) {
			continue;
		}
		if (identity_decision != 1) {
			*out_stage = MARKER_STAGE_IDENTITY;
			free(fds);
			free(pids);
			free(holders);
			return marker_errno_or_eio(identity_errno);
		}
		errno = 0;
		int needed = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
		int fd_errno = errno;
		if (needed <= 0) {
			if (marker_vanished(needed, fd_errno)) {
				continue;
			}
			*out_stage = MARKER_STAGE_FDLIST;
			free(fds);
			free(pids);
			free(holders);
			return marker_errno_or_eio(fd_errno);
		}
		int capacity = needed + 64 * (int)sizeof(struct proc_fdinfo);
		if (capacity > fd_capacity) {
			void *grown = realloc(fds, (size_t)capacity);
			if (grown == NULL) {
				*out_stage = MARKER_STAGE_ALLOC;
				free(fds);
				free(pids);
				free(holders);
				return ENOMEM;
			}
			fds = grown;
			fd_capacity = capacity;
		}
		int bytes;
		int bytes_errno;
		int fd_attempts = 0;
		for (;;) {
			errno = 0;
			bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, fd_capacity);
			bytes_errno = errno;
			int fd_decision = marker_capacity_decision(bytes, fd_capacity,
				bytes_errno, &fd_attempts);
			if (fd_decision == 0) {
				break;
			}
			if (fd_decision == EOVERFLOW) {
				*out_stage = MARKER_STAGE_FDLIST;
				free(fds);
				free(pids);
				free(holders);
				return EOVERFLOW;
			}
			if (fd_decision != 1) {
				break;
			}
			int next_fd_capacity = fd_capacity * 2;
			void *grown = realloc(fds, (size_t)next_fd_capacity);
			if (grown == NULL) {
				*out_stage = MARKER_STAGE_ALLOC;
				free(fds);
				free(pids);
				free(holders);
				return ENOMEM;
			}
			fds = grown;
			fd_capacity = next_fd_capacity;
		}
		if (bytes != fd_capacity && marker_vanished(bytes, bytes_errno)) {
			continue;
		}
		if (bytes <= 0 || bytes > fd_capacity || bytes % (int)sizeof(struct proc_fdinfo) != 0) {
			*out_stage = MARKER_STAGE_FDLIST;
			free(fds);
			free(pids);
			free(holders);
			return marker_errno_or_eio(bytes_errno);
		}
		int fd_count = bytes / (int)sizeof(struct proc_fdinfo);
		for (int j = 0; j < fd_count; j++) {
			int deadline_error = 0;
			if (marker_deadline_expired(started, budget_ns, &deadline_error)) {
				*out_stage = deadline_error == ETIMEDOUT ? MARKER_STAGE_DEADLINE : MARKER_STAGE_CLOCK;
				free(fds);
				free(pids);
				free(holders);
				return deadline_error;
			}
			if (fds[j].proc_fdtype != PROX_FDTYPE_VNODE) {
				continue;
			}
			struct vnode_fdinfowithpath vnode;
			errno = 0;
			int vnode_bytes = proc_pidfdinfo(pid, fds[j].proc_fd,
				PROC_PIDFDVNODEPATHINFO, &vnode, (int)sizeof(vnode));
			int vnode_errno = errno;
			if (vnode_bytes != (int)sizeof(vnode) && marker_vanished(vnode_bytes, vnode_errno)) {
				continue;
			}
			if (vnode_bytes != (int)sizeof(vnode)) {
				*out_stage = MARKER_STAGE_VNODE;
				free(fds);
				free(pids);
				free(holders);
				return marker_errno_or_eio(vnode_errno);
			}
			if (vnode.pvip.vip_vi.vi_stat.vst_dev == want.st_dev &&
				vnode.pvip.vip_vi.vi_stat.vst_ino == want.st_ino) {
				struct proc_bsdinfo identity_after;
				errno = 0;
				identity_bytes = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0,
					&identity_after, (int)sizeof(identity_after));
				identity_errno = errno;
				int recheck_decision = marker_identity_decision(identity_bytes,
					(int)sizeof(identity_after), identity_errno,
					identity_bytes == (int)sizeof(identity_after) &&
					identity_after.pbi_pid == identity_before.pbi_pid &&
					identity_after.pbi_start_tvsec == identity_before.pbi_start_tvsec &&
					identity_after.pbi_start_tvusec == identity_before.pbi_start_tvusec);
				if (recheck_decision == 0) {
					break;
				}
				if (recheck_decision != 1) {
					*out_stage = MARKER_STAGE_IDENTITY_RECHECK;
					free(fds);
					free(pids);
					free(holders);
					return marker_errno_or_eio(identity_errno);
				}
				holders[holder_count].pid = pid;
				holders[holder_count].start_sec = (int64_t)identity_before.pbi_start_tvsec;
				holders[holder_count].start_usec = (int64_t)identity_before.pbi_start_tvusec;
				holder_count++;
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

type libprocInspectionError struct {
	stage string
	errno syscall.Errno
}

func (e *libprocInspectionError) Error() string {
	return fmt.Sprintf("libproc %s: %v", e.stage, e.errno)
}

func (e *libprocInspectionError) Unwrap() error { return e.errno }

type markerHolder struct {
	pid       int
	startSec  int64
	startUsec int64
}

type markerHolderScan func(string, time.Duration) ([]markerHolder, error)

var markerHoldersFn markerHolderScan = markerHoldersNative

func libprocStage(stage C.int) string {
	switch int(stage) {
	case C.MARKER_STAGE_STAT:
		return "stat"
	case C.MARKER_STAGE_CLOCK:
		return "clock"
	case C.MARKER_STAGE_PIDLIST:
		return "pid inventory"
	case C.MARKER_STAGE_IDENTITY:
		return "identity"
	case C.MARKER_STAGE_FDLIST:
		return "fd list"
	case C.MARKER_STAGE_VNODE:
		return "vnode"
	case C.MARKER_STAGE_IDENTITY_RECHECK:
		return "identity recheck"
	case C.MARKER_STAGE_ALLOC:
		return "allocation"
	case C.MARKER_STAGE_DEADLINE:
		return "deadline"
	case C.MARKER_STAGE_OUTPUT:
		return "native output"
	default:
		return "unknown"
	}
}

// libprocVanishedForTest exposes the compiled-C errno boundary to focused
// tests without touching host process state.
func libprocVanishedForTest(result int, errno syscall.Errno) bool {
	return C.marker_vanished(C.int(result), C.int(errno)) != 0
}

func libprocErrnoOrEIOForTest(errno syscall.Errno) syscall.Errno {
	return syscall.Errno(C.marker_errno_or_eio(C.int(errno)))
}

func libprocClockFailureForTest(errno syscall.Errno) syscall.Errno {
	return syscall.Errno(C.marker_clock_failure_for_test(C.int(errno)))
}

func libprocDeadlineResultForTest(errno syscall.Errno) syscall.Errno {
	return syscall.Errno(C.marker_deadline_result_for_test(C.int(errno)))
}

func markerOutputDecisionForTest(hasHolders bool, holderCount int) bool {
	has := C.int(0)
	if hasHolders {
		has = 1
	}
	return C.marker_output_decision(has, C.int(holderCount)) != 0
}

func markerCapacityDecisionForTest(result, capacity int, savedErrno syscall.Errno, attempts int) (int, int) {
	cAttempts := C.int(attempts)
	decision := C.marker_capacity_decision(C.int(result), C.int(capacity),
		C.int(savedErrno), &cAttempts)
	return int(decision), int(cAttempts)
}

func markerIdentityDecisionForTest(result, expected int, savedErrno syscall.Errno, matches bool) int {
	identityMatches := C.int(0)
	if matches {
		identityMatches = 1
	}
	return int(C.marker_identity_decision(C.int(result), C.int(expected),
		C.int(savedErrno), identityMatches))
}

func markerHoldersNative(markerPath string, budget time.Duration) ([]markerHolder, error) {
	cPath := C.CString(markerPath)
	defer C.free(unsafe.Pointer(cPath))
	var holders *C.struct_marker_holder
	var holderCount C.int
	var stage C.int
	code := C.marker_holders(cPath, C.int64_t(budget.Nanoseconds()), &holders, &holderCount, &stage)
	if code != 0 {
		return nil, &libprocInspectionError{stage: libprocStage(stage), errno: syscall.Errno(code)}
	}
	hasHolders := C.int(0)
	if holders != nil {
		hasHolders = 1
	}
	if C.marker_output_decision(hasHolders, holderCount) == 0 {
		if holders != nil {
			C.free(unsafe.Pointer(holders))
		}
		return nil, &libprocInspectionError{stage: "native output", errno: syscall.EIO}
	}
	if holders == nil || holderCount == 0 {
		return nil, nil
	}
	defer C.free(unsafe.Pointer(holders))
	native := unsafe.Slice(holders, int(holderCount))
	out := make([]markerHolder, 0, len(native))
	for _, holder := range native {
		out = append(out, markerHolder{
			pid:       int(holder.pid),
			startSec:  int64(holder.start_sec),
			startUsec: int64(holder.start_usec),
		})
	}
	return out, nil
}

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
	native, err := markerHoldersFn(markerPath, remaining)
	if err != nil {
		return nil, fmt.Errorf("processesHoldingMarker libproc: %w", err)
	}
	if len(native) == 0 {
		return nil, nil
	}

	excl := residualExcludePIDs()
	out := make([]procToken, 0, len(native))
	seen := make(map[int]struct{}, len(native))
	for _, holder := range native {
		pid := holder.pid
		if pid <= 1 {
			continue
		}
		if _, skip := excl[pid]; skip {
			continue
		}
		tok := procToken{pid: pid, startSec: holder.startSec, startUsec: holder.startUsec}
		if _, ok := seen[tok.pid]; ok {
			continue
		}
		seen[tok.pid] = struct{}{}
		out = append(out, tok)
	}
	return out, nil
}
