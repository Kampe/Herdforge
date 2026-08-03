//go:build darwin && cgo

package verifier

import (
	"bytes"
	"context"
	"errors"
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
		{name: "identity EPERM", stage: "identity", errno: syscall.EPERM},
		{name: "identity EACCES", stage: "identity", errno: syscall.EACCES},
		{name: "identity unexpected", stage: "identity", errno: syscall.EIO},
		{name: "fd list EPERM", stage: "fd list", errno: syscall.EPERM},
		{name: "fd list EACCES", stage: "fd list", errno: syscall.EACCES},
		{name: "fd list unexpected", stage: "fd list", errno: syscall.ENOSPC},
		{name: "vnode EPERM", stage: "vnode", errno: syscall.EPERM},
		{name: "vnode EACCES", stage: "vnode", errno: syscall.EACCES},
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
		return nil, &libprocInspectionError{stage: "identity", errno: syscall.EPERM}
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
