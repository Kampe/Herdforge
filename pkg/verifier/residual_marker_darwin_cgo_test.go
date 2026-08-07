//go:build darwin && cgo

package verifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestNativeDeadlineCallsitesReturnPropagatedErrno(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "residual_marker_darwin_cgo.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(source, []byte("return deadline_error;")); got != 2 {
		t.Fatalf("native deadline callsites returning deadline_error = %d, want 2", got)
	}
}

func TestLibprocInspectionSeamPreservesVanishedSkip(t *testing.T) {
	previous := markerHoldersFn
	t.Cleanup(func() { markerHoldersFn = previous })
	markerHoldersFn = func(string, time.Duration) ([]markerHolder, error) {
		// Native libproc converts the contract-qualified ESRCH/EIO race into an
		// empty successful observation, allowing the fixed-point loop to rescan.
		return nil, nil
	}
	if _, err := processesHoldingMarkerUntil("marker", time.Now().Add(time.Second)); err != nil {
		t.Fatalf("vanished process race must be skippable: %v", err)
	}
}

func TestCompiledLibprocVanishedClassificationIsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  int
		errno   syscall.Errno
		expects bool
	}{
		{name: "zero EPERM", result: 0, errno: syscall.EPERM},
		{name: "zero EACCES", result: 0, errno: syscall.EACCES},
		{name: "short positive stale EIO", result: 8, errno: syscall.EIO},
		{name: "zero ESRCH", result: 0, errno: syscall.ESRCH, expects: true},
		{name: "zero EIO", result: 0, errno: syscall.EIO, expects: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := libprocVanishedForTest(tc.result, tc.errno); got != tc.expects {
				t.Fatalf("vanished classification = %v, want %v", got, tc.expects)
			}
		})
	}
}

func TestCompiledLibprocVnodeEBADFOnlySkipsIndividualClosedFD(t *testing.T) {
	if libprocVanishedForTest(-1, syscall.EBADF) {
		t.Fatal("generic libproc vanished classification must remain fail-closed for EBADF")
	}

	tests := []struct {
		name   string
		stage  int
		result int
		errno  syscall.Errno
		want   bool
	}{
		{name: "vnode negative EBADF", stage: 6, result: -1, errno: syscall.EBADF, want: true},
		{name: "vnode zero EBADF", stage: 6, result: 0, errno: syscall.EBADF, want: true},
		{name: "vnode malformed positive EBADF", stage: 6, result: 1, errno: syscall.EBADF},
		{name: "vnode negative EPERM", stage: 6, result: -1, errno: syscall.EPERM},
		{name: "vnode negative EACCES", stage: 6, result: -1, errno: syscall.EACCES},
		{name: "identity negative EBADF", stage: 4, result: -1, errno: syscall.EBADF},
		{name: "fd list negative EBADF", stage: 5, result: -1, errno: syscall.EBADF},
		{name: "stat negative EBADF", stage: 1, result: -1, errno: syscall.EBADF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := libprocStageVanishedForTest(tc.stage, tc.result, tc.errno); got != tc.want {
				t.Fatalf("stage vanished=%v, want %v (stage=%d result=%d errno=%v)",
					got, tc.want, tc.stage, tc.result, tc.errno)
			}
		})
	}
}

func TestCompiledLibprocCapacityAndIdentityDecisions(t *testing.T) {
	if got := libprocErrnoOrEIOForTest(0); got != syscall.EIO {
		t.Fatalf("errno-zero native failure must become EIO: %v", got)
	}
	if got := libprocErrnoOrEIOForTest(syscall.EPERM); got != syscall.EPERM {
		t.Fatalf("classified native errno must be preserved: %v", got)
	}
	if got := libprocClockFailureForTest(0); got != syscall.EIO {
		t.Fatalf("clock errno-zero failure must become EIO: %v", got)
	}
	if got := libprocClockFailureForTest(syscall.EACCES); got != syscall.EACCES {
		t.Fatalf("clock errno must be preserved: %v", got)
	}
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{name: "clock EIO", errno: syscall.EIO},
		{name: "clock EACCES", errno: syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := libprocDeadlineResultForTest(tc.errno); got != tc.errno {
				t.Fatalf("deadline native error = %v, want injected %v", got, tc.errno)
			}
		})
	}
	if got := libprocDeadlineResultForTest(0); got != syscall.EIO {
		t.Fatalf("errno-zero deadline failure must become EIO: %v", got)
	}
	const retry = 1
	if decision, attempts := markerCapacityDecisionForTest(8, 8, 0, 0); decision != retry || attempts != 1 {
		t.Fatalf("full PID/FD buffer must retry: decision=%d attempts=%d", decision, attempts)
	}
	if decision, attempts := markerCapacityDecisionForTest(8, 8, 0, 3); decision != int(syscall.EOVERFLOW) || attempts != 4 {
		t.Fatalf("fourth full buffer must fail closed: decision=%d attempts=%d", decision, attempts)
	}
	if decision, attempts := markerCapacityDecisionForTest(0, 8, syscall.EPERM, 0); decision != int(syscall.EPERM) || attempts != 0 {
		t.Fatalf("zero+EPERM inventory must block: decision=%d attempts=%d", decision, attempts)
	}
	if decision, _ := markerCapacityDecisionForTest(0, 8, 0, 0); decision != int(syscall.EIO) {
		t.Fatalf("zero+errno-zero inventory must become EIO: decision=%d", decision)
	}
	if decision, _ := markerCapacityDecisionForTest(4, 8, syscall.EIO, 0); decision != int(syscall.EIO) {
		t.Fatalf("positive+EIO capacity result must block: decision=%d", decision)
	}
	if decision, _ := markerCapacityDecisionForTest(4, 8, syscall.EPERM, 0); decision != int(syscall.EPERM) {
		t.Fatalf("positive+EPERM capacity result must block: decision=%d", decision)
	}
	if decision := markerIdentityDecisionForTest(136, 136, 0, false); decision != int(syscall.EIO) {
		t.Fatalf("identity mismatch must block: decision=%d", decision)
	}
	if decision := markerIdentityDecisionForTest(136, 136, 0, true); decision != 1 {
		t.Fatalf("matching identity must accept: decision=%d", decision)
	}
	if decision := markerIdentityDecisionForTest(136, 136, syscall.EIO, true); decision != int(syscall.EIO) {
		t.Fatalf("exact-size identity with errno must block: decision=%d", decision)
	}
	if decision := markerIdentityDecisionForTest(0, 136, syscall.ESRCH, false); decision != 0 {
		t.Fatalf("zero+ESRCH identity race must skip: decision=%d", decision)
	}
	if decision := markerIdentityDecisionForTest(136, 136, 0, false); decision != int(syscall.EIO) {
		t.Fatalf("identity recheck change must block: decision=%d", decision)
	}
	for _, tc := range []struct {
		has   bool
		count int
		ok    bool
	}{
		{has: false, count: 0, ok: true},
		{has: true, count: 0, ok: false},
		{has: false, count: 1, ok: false},
		{has: true, count: 1, ok: true},
		{has: false, count: -1, ok: false},
	} {
		if got := markerOutputDecisionForTest(tc.has, tc.count); got != tc.ok {
			t.Fatalf("holder output consistency (%v,%d)=%v, want %v", tc.has, tc.count, got, tc.ok)
		}
	}
}

func TestLibprocInspectionSeamBlocksUnexpectedErrorsByStage(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		errno syscall.Errno
	}{
		{name: "identity unexpected", stage: "identity", errno: syscall.EIO},
		{name: "fd list unexpected", stage: "fd list", errno: syscall.ENOSPC},
		{name: "vnode unexpected", stage: "vnode", errno: syscall.EBADF},
		{name: "truncated PID inventory", stage: "pid inventory", errno: syscall.EOVERFLOW},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := markerHoldersFn
			t.Cleanup(func() { markerHoldersFn = previous })
			markerHoldersFn = func(string, time.Duration) ([]markerHolder, error) {
				return nil, &libprocInspectionError{stage: tc.stage, errno: tc.errno}
			}
			_, err := processesHoldingMarkerUntil("marker", time.Now().Add(time.Second))
			if err == nil {
				t.Fatal("unexpected inspection failure was accepted")
			}
			var inspectionErr *libprocInspectionError
			if !errors.As(err, &inspectionErr) {
				t.Fatalf("error must retain typed libproc cause: %v", err)
			}
			if inspectionErr.stage != tc.stage || !errors.Is(err, tc.errno) {
				t.Fatalf("wrong typed failure: stage=%q errno=%v error=%v", inspectionErr.stage, inspectionErr.errno, err)
			}
		})
	}
}

func TestLibprocInspectionFailureMakesVerifierBlocked(t *testing.T) {
	previousDrain := markerLineageDrainedFn
	previousScan := markerHoldersFn
	t.Cleanup(func() {
		markerLineageDrainedFn = previousDrain
		markerHoldersFn = previousScan
	})
	markerLineageDrainedFn = func(string) (bool, error) { return false, nil }
	markerHoldersFn = func(string, time.Duration) ([]markerHolder, error) {
		return nil, &libprocInspectionError{stage: "identity", errno: syscall.EIO}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.Outcome != OutcomeBLOCKED {
		t.Fatalf("uninspectable marker holder must BLOCKED: %+v", receipt)
	}
}

func TestDarwinPermissionFallbackParsesSpacedMarkerAndBindsToken(t *testing.T) {
	previousHolders := markerHoldersFn
	previousLsof := markerLsofOutputFn
	previousToken := markerTokenOfFn
	t.Cleanup(func() {
		markerHoldersFn = previousHolders
		markerLsofOutputFn = previousLsof
		markerTokenOfFn = previousToken
	})
	markerPath := filepath.Join(t.TempDir(), "repo with spaces", "marker file")
	want := procToken{pid: 4242, startSec: 77, startUsec: 88}
	markerHoldersFn = func(string, time.Duration) ([]markerHolder, error) {
		return nil, fmt.Errorf("wrapped permission failure: %w", syscall.EPERM)
	}
	markerTokenOfFn = func(pid int) (procToken, error) {
		if pid != want.pid {
			return procToken{}, fmt.Errorf("unexpected token lookup pid %d", pid)
		}
		return want, nil
	}
	abs, err := filepath.Abs(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	markerLsofOutputFn = func(string, time.Time) ([]byte, error) {
		return []byte(fmt.Sprintf("p%d\nn%s\npnot-a-pid\nn%s\np%d\nn%s\n",
			os.Getpid(), abs, abs, want.pid, abs)), nil
	}
	got, err := processesHoldingMarkerUntil(markerPath, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].equal(want) {
		t.Fatalf("permission fallback tokens = %+v, want exact %+v", got, want)
	}
}

func TestDarwinPermissionFallbackRejectsNonPermissionAndDeadline(t *testing.T) {
	previousHolders := markerHoldersFn
	previousLsof := markerLsofOutputFn
	t.Cleanup(func() {
		markerHoldersFn = previousHolders
		markerLsofOutputFn = previousLsof
	})
	lsofCalls := 0
	markerLsofOutputFn = func(string, time.Time) ([]byte, error) {
		lsofCalls++
		return nil, errors.New("lsof seam must not run")
	}
	markerHoldersFn = func(string, time.Duration) ([]markerHolder, error) {
		return nil, fmt.Errorf("wrapped inspection failure: %w", syscall.EIO)
	}
	if _, err := processesHoldingMarkerUntil("marker", time.Now().Add(time.Second)); err == nil {
		t.Fatal("non-permission native failure must remain fail-closed")
	}
	if lsofCalls != 0 {
		t.Fatalf("non-permission native failure invoked lsof fallback %d time(s)", lsofCalls)
	}
	if _, err := processesHoldingMarkerViaLsof("marker", time.Now().Add(-time.Millisecond)); err == nil {
		t.Fatal("expired lsof deadline must fail before runner invocation")
	}
	if lsofCalls != 0 {
		t.Fatalf("expired lsof deadline invoked runner %d time(s)", lsofCalls)
	}
}
