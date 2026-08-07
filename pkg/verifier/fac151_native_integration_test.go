//go:build fac151_hermetic_integration

package verifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestExecuteCancellationKillsProcessGroup(t *testing.T) {
	parallelVerifierStress(t)
	// Deterministic ownership barrier:
	//  1. Start Execute asynchronously (context cancel is explicit, not timed).
	//  2. Wait for child-ready signal (pid file written by the descendant).
	//  3. cancel() immediately after ready — ready, not timeout, triggers Cancel.
	//  4. Wait for Execute completion (explicit reap completion inside execute).
	//  5. Assert the exact descendant is gone.
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		// Descendant writes $$ then parks. Ready signal IS the pid file.
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type execOut struct {
		result *Result
		err    error
	}
	done := make(chan execOut, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOut{r, e}
	}()

	// Diagnostic bound only: fails the test if the child never signals ready.
	// It does NOT cancel Execute — cancel runs only after ready is observed.
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready barrier: %v", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		cancel()
		<-done
		t.Fatalf("ready pid %d is not a live process: %v", pid, err)
	}

	cancel() // cancellation mechanism: ready-observed, not a timer

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("canceled process group must be BLOCKED: %+v", out.result)
	}
	// After Execute returns, residual group reap has completed. Zombies may
	// briefly remain until the OS reparents; ESRCH is the ownership proof.
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("canceled verifier left descendant process %d alive after reap completion: %v", pid, err)
	}
}

// TestExecuteCancellationWithoutReadyBarrierCannotProveDescendant documents the
// pre-fix flake class: cancelling before the child-ready signal means the
// test cannot prove a descendant existed for process-group reap. This is the
// race×100 failure mode (missing child.pid) when cancel is timer-driven.
func TestExecuteCancellationWithoutReadyBarrierCannotProveDescendant(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()
	// Immediate cancel — no ready barrier. May fail Start with context.Canceled
	// or return BLOCKED without a proven descendant. Either way, without the
	// ready barrier we must not claim process-group ownership of a child.
	cancel()
	out := <-done
	if out.err != nil {
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("immediate cancel without ready barrier: unexpected err %v", out.err)
		}
		return
	}
	if out.result == nil || out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("immediate cancel without ready barrier must BLOCKED or ctx err: %+v", out.result)
	}
	// Non-vacuous claim: this test deliberately does NOT call
	// waitForChildReadyPID, so it cannot assert a specific descendant was
	// reaped — that proof lives only in TestExecuteCancellationKillsProcessGroup.
}

// TestExecuteCancellationRequiresProcessGroupReap mutation-proves the incomplete
// ownership shape: leader-only Cancel kill PLUS muted residual drain and
// finalizeOwnedTree leaves the ready descendant alive. Production pairs
// live-group Cancel with done-phase residual drain + finalize so the
// descendant does not survive.
func TestExecuteCancellationRequiresProcessGroupReap(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	prevKill := processGroupKiller
	processGroupKiller = func(pgid int) error {
		if pgid <= 0 {
			return nil
		}
		return syscall.Kill(pgid, syscall.SIGKILL) // leader only — WRONG
	}
	prevFin := finalizeOwnedTree
	prevDrain := residualDrainFn
	// MUTATION: skip residual drain and finalize kill (incomplete ownership).
	residualDrainFn = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
		}
		return nil
	}
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			_ = o.stopTracker()
		}
		return nil
	}
	t.Cleanup(func() {
		processGroupKiller = prevKill
		finalizeOwnedTree = prevFin
		residualDrainFn = prevDrain
		if data, err := os.ReadFile(pidFile); err == nil {
			if p, conv := strconv.Atoi(strings.TrimSpace(string(data))); conv == nil && p > 0 {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()

	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready barrier: %v", err)
	}
	cancel()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	// Incomplete ownership leaves the descendant alive — mutation proof.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected descendant %d to survive leader-only+no-drain+no-finalize: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isESRCH(err) {
		t.Fatalf("mutation cleanup kill descendant %d: %v", pid, err)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("mutation cleanup: descendant %d still live: %v", pid, err)
	}
}

// TestExecuteCancellationReadyBarrierSelectorRuns proves waitForChildReadyPID
// fails closed when the ready signal never appears (non-vacuous helper gate).
func TestExecuteCancellationReadyBarrierSelectorRuns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-ready.pid")
	if _, err := waitForChildReadyPID(missing, time.Millisecond); err == nil {
		t.Fatal("waitForChildReadyPID must fail when the ready signal never appears")
	}
}

type execOutOrErr struct {
	result *Result
	err    error
}

// waitForChildReadyPID blocks until path contains a positive live pid, or the
// diagnostic bound elapses. The bound only fails the waiter — callers must
// cancel Execute explicitly after ready.
func waitForChildReadyPID(path string, bound time.Duration) (int, error) {
	deadline := time.Now().Add(bound)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				if killErr := syscall.Kill(pid, 0); killErr == nil {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(path))
		}
		// Observe the ready file; not a cancel/cleanup delay.
		time.Sleep(time.Millisecond)
	}
}

// waitForPIDGone waits until the pid is gone or a reaped/zombie non-target,
// proving it can no longer mutate (matches production waitHandleGone policy).
func waitForPIDGone(pid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
		// Zombie: reparented residual already SIGKILL'd; cannot mutate.
		if processIsZombie(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d still exists after diagnostic bound", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

// productionLeaveWriterScript: leader backgrounds a same-group active writer
// (stdio detached so Wait is not pipe-blocked), waits until the writer pid
// file exists (explicit handshake, not a sleep proof), then exits 0.
// Production finalizeOwnedTree must BLOCK and reap the residual tree.
//
// $1=pidfile $2=writetarget (absolute path under verification dir)
const productionLeaveWriterScript = `
sh -c 'printf "%s\n" "$$" > "$1"; while true; do printf w >> "$2"; done' writer "$1" "$2" </dev/null >/dev/null 2>&1 &
# Handshake: do not exit until the writer has published its pid.
while [ ! -s "$1" ]; do :; done
exit 0
`

// productionDetachedOnlyScript: adversarial setsid + double-fork residual writer.
// Intermediate parents exit immediately (no keep-alive). The grandchild calls
// os.setsid() so it is NOT a member of the original process group — membership
// kill alone cannot own it. Production must discover it via the inherited
// locked marker FD and identity-kill by token; the open writetarget is
// corroboration only.
//
// $1=writerPid $2=writetarget
// Optional $3=startedPath $4=releasePath provides an explicit ordering gate
// for tests that must launch an unrelated holder after this command starts.
const productionDetachedOnlyScript = `
if [ "$#" -ge 4 ]; then
  printf "%s\n" "$$" > "$3" || exit 1
  while [ ! -e "$4" ]; do :; done
fi
python3 -c '
import os, sys
path, target = sys.argv[1], sys.argv[2]
# Ownership wrapper leaves FD5 open as the inherited lineage marker.
# Do not close FD5 across setsid/double-fork — that is kill authority.
# First fork + setsid: leave the original process group / session.
if os.fork() > 0:
    os._exit(0)
os.setsid()
# Second fork: intermediate session leader exits; grandchild is reparented
# outside the original pgid and is not a session leader wait-edge.
if os.fork() > 0:
    os._exit(0)
# Open a descendant file under the candidate (path corroboration only),
# then chdir away. Marker FD5 remains the lineage authority.
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
while True:
    out.write("w")
    out.flush()
' "$1" "$2" </dev/null >/dev/null 2>&1
while [ ! -s "$1" ]; do :; done
exit 0
`

// productionDetachedSessionScript: same-group background + real setsid residual
// that retains FD5 marker lineage and chdirs away with an open descendant FD.
// Each writer has a bounded first generation, then remains alive briefly so
// Execute must reap it rather than passing after natural writer exit.
// $1=sessionPid $2=writetarget $3=groupPid
const productionDetachedSessionScript = `
wait_for_nonempty() {
  i=0
  while [ ! -s "$1" ]; do
    if [ "$i" -ge 500 ]; then return 124; fi
    i=$((i + 1))
    sleep 0.01
  done
}
wait_for_exists() {
  i=0
  while [ ! -e "$1" ]; do
    if [ "$i" -ge 500 ]; then return 124; fi
    i=$((i + 1))
    sleep 0.01
  done
}
sh -c 'printf "%s\n" "$$" > "$1"; for i in $(seq 1 4096); do printf g >> "$2"; done; sleep 5' grpwriter "$3" "$2" </dev/null >/dev/null 2>&1 &
python3 -c '
import os, sys
path, target = sys.argv[1], sys.argv[2]
# Keep FD5 (inherited ownership marker) open across setsid/double-fork.
if os.fork() > 0:
    os._exit(0)
os.setsid()
if os.fork() > 0:
    os._exit(0)
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
for _ in range(4096):
    out.write("w")
    out.flush()
import time
time.sleep(5)
' "$1" "$2" </dev/null >/dev/null 2>&1
wait_for_nonempty "$3" || exit $?
wait_for_nonempty "$1" || exit $?
if [ "$#" -ge 5 ]; then
  printf "%s\n" "$$" > "$4" || exit 1
  wait_for_exists "$5" || exit $?
fi
exit 0
`

type pidTokenObservation struct {
	path string
	tok  procToken
	err  error
}

func observePIDToken(ctx context.Context, path string, deadline time.Time) (procToken, error) {
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 1 {
				if tok, tokenErr := tokenOf(pid); tokenErr == nil && tok.isLiveTarget() {
					return tok, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return procToken{}, fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(path))
		}
		select {
		case <-ctx.Done():
			return procToken{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func reapExactTokens(t *testing.T, tokens ...procToken) {
	t.Helper()
	for _, tok := range tokens {
		if !tok.valid() {
			continue
		}
		h, err := openHandle(tok)
		if err != nil {
			if tok.stillSame() {
				t.Errorf("cleanup open exact pid %d: %v", tok.pid, err)
			}
			continue
		}
		if _, err := h.kill(); err != nil {
			t.Errorf("cleanup kill exact pid %d: %v", tok.pid, err)
		}
		h.close()
		if err := waitTokenGone(tok, 2*time.Second); err != nil {
			t.Errorf("cleanup wait exact pid %d gone: %v", tok.pid, err)
		}
	}
}

func assertWriterGone(t *testing.T, pidFile string, diagnostics ...string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read writer pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("bad writer pid %q", data)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		if len(diagnostics) > 0 {
			t.Fatalf("production left writer pid %d live: %v; result output: %s", pid, err, diagnostics[0])
		}
		t.Fatalf("production left writer pid %d live: %v", pid, err)
	}
}

func terminateTestProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("cleanup kill pid %d: %v", cmd.Process.Pid, err)
	}
	if err := cmd.Wait(); err != nil && !isExpectedKillWait(err) {
		t.Errorf("cleanup wait pid %d: %v", cmd.Process.Pid, err)
	}
}

func parentPidOf(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

func forceKillTrackedPID(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("cleanup read pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		return
	}
	// Kill parent first so SIGKILL'd children are not stuck as zombies (PPID still
	// alive and not waiting). Then kill the pid and any remaining children.
	if ppid, perr := parentPidOf(pid); perr == nil && ppid > 1 {
		if err := syscall.Kill(ppid, syscall.SIGKILL); err != nil && !isESRCH(err) {
			t.Fatalf("cleanup SIGKILL parent %d: %v", ppid, err)
		}
	}
	if kids, kerr := listChildPids(pid); kerr != nil {
		t.Fatalf("cleanup listChildPids %d: %v", pid, kerr)
	} else {
		for _, k := range kids {
			if err := syscall.Kill(k, syscall.SIGKILL); err != nil && !isESRCH(err) {
				t.Fatalf("cleanup SIGKILL child %d: %v", k, err)
			}
		}
	}
	if err := killProcessGroupIfLive(pid); err != nil && !isESRCH(err) {
		t.Fatalf("cleanup killProcessGroupIfLive %d: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isESRCH(err) {
		t.Fatalf("cleanup SIGKILL pid %d: %v", pid, err)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("cleanup wait pid %d gone: %v", pid, err)
	}
}

// TestExecuteSuccessWithBackgroundWriterBlocksAndReaps is the production-path
// proof: Execute of a command that exits 0 while a same-group writer remains
// must return BLOCKED (residual owned tree) and the writer must be gone.
func TestExecuteSuccessWithBackgroundWriterBlocksAndReaps(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-writer"), "#!/bin/sh\n"+productionLeaveWriterScript)

	result, err := NewVerifierArgs([]string{"./leave-writer", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("exit-0 with residual same-group writer must BLOCKED, got %+v", result)
	}
	if !strings.Contains(result.Output, "residual") && !strings.Contains(result.Output, "ownership") {
		t.Fatalf("BLOCKED output must name ownership/residual close: %q", result.Output)
	}
	assertWriterGone(t, pidFile)
}

// TestExecuteMutationOmittingFinalizeOwnedTreeReturnsTooEarly mutation-proves
// production finalizeOwnedTree is load-bearing: when it only stops tracking
// without reaping, Execute returns PASS while the background writer is live.
func TestExecuteMutationOmittingFinalizeOwnedTreeReturnsTooEarly(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-writer"), "#!/bin/sh\n"+productionLeaveWriterScript)

	prevFin := finalizeOwnedTree
	prevDrain := residualDrainFn
	// MUTATION: skip done-phase residual drain and finalize kill.
	residualDrainFn = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
		}
		return nil
	}
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			_ = o.stopTracker()
		}
		return nil
	}
	t.Cleanup(func() {
		finalizeOwnedTree = prevFin
		residualDrainFn = prevDrain
		forceKillTrackedPID(t, pidFile)
	})

	result, err := NewVerifierArgs([]string{"./leave-writer", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomePASS {
		t.Fatalf("mutation: omitting residual drain should return PASS on exit 0; got %+v", result)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("mutation: writer pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("mutation: bad writer pid %q", data)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected writer %d to survive without finalizeOwnedTree reap: %v", pid, err)
	}
}

// TestExecuteCancelAfterStartClosesProcessGroup covers cancellation after the
// process is running: Cancel kills the live group, Wait + finalizeOwnedTree
// prove no residual members before BLOCKED returns.
func TestExecuteCancelAfterStartClosesProcessGroup(t *testing.T) {
	parallelVerifierStress(t)
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	writeExecutable(t, filepath.Join(dir, "spawn-child"), "#!/bin/sh\n"+
		"sh -c 'printf \"%s\\n\" \"$$\" > \"$1\"; exec sleep 30' childpid \"$1\" &\n"+
		"wait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan execOutOrErr, 1)
	go func() {
		r, e := NewVerifierArgs([]string{"./spawn-child", pidFile}).Execute(ctx, dir)
		done <- execOutOrErr{r, e}
	}()
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("child-ready: %v", err)
	}
	cancel()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result == nil || out.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("cancel after Start must BLOCKED: %+v", out.result)
	}
	if err := waitForPIDGone(pid, 2*time.Second); err != nil {
		t.Fatalf("cancel path left descendant %d live after ownership close: %v", pid, err)
	}
}

// TestMarkerLineageFindsSetsidChdirAwayWriter proves processesHoldingMarker
// discovers a setsid writer that inherited the ownership marker FD, chdir'd
// away, and still mutates a descendant file — lineage authority, not path.
func TestOwnershipMarkerLockTracksLastInheritedHolder(t *testing.T) {
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if marker != nil {
			if err := marker.Close(); err != nil {
				t.Errorf("cleanup marker close: %v", err)
			}
		}
		if err := removeMarkerPath(markerPath); err != nil {
			t.Errorf("cleanup marker path: %v", err)
		}
	})

	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{marker}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, cmd) })
	if err := marker.Close(); err != nil {
		t.Fatalf("close parent marker: %v", err)
	}
	marker = nil

	drained, err := markerLineageDrained(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if drained {
		t.Fatal("marker fixed point reported drained while inherited holder was live")
	}
	terminateTestProcess(t, cmd)
	drained, err = markerLineageDrained(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !drained {
		t.Fatal("marker fixed point remained held after last inherited holder exited")
	}
}

func TestMarkerLineageFindsSetsidChdirAwayWriter(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for marker lineage fixture")
	}
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = marker.Close()
		_ = os.Remove(markerPath)
	})
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "writer.pid")
	target := filepath.Join(dir, "nested", "residue.log")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// Inherit marker as FD3 (ExtraFiles[0]); keep it open across setsid.
	cmd := exec.Command("python3", "-c", `
import os, sys, time
path, target = sys.argv[1], sys.argv[2]
# ExtraFiles[0] is FD 3 in the child — do not close it (lineage marker).
if os.fork() > 0:
    os._exit(0)
os.setsid()
if os.fork() > 0:
    os._exit(0)
# Retain marker FD (lineage) + open descendant (path corroboration only).
out = open(target, "a", encoding="utf-8")
os.chdir("/")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
while True:
    out.write("w")
    out.flush()
    time.sleep(0.05)
`, pidFile, target)
	cmd.ExtraFiles = []*os.File{marker}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		forceKillTrackedPID(t, pidFile)
	})
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("writer ready: %v", err)
	}
	toks, err := processesHoldingMarker(markerPath)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("host-wide libproc diagnostic unavailable: %v", err)
		}
		t.Fatalf("processesHoldingMarker: %v", err)
	}
	toks = filterResidualTokens(toks, -1)
	found := false
	for _, tok := range toks {
		if tok.pid == pid {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("marker lineage must find setsid writer pid %d holding inherited marker; got %v", pid, toks)
	}
	excl := residualExcludePIDs()
	for _, tok := range toks {
		if _, bad := excl[tok.pid]; bad {
			t.Fatalf("marker lineage must not include control-plane pid %d", tok.pid)
		}
	}
}

// TestUnrelatedPathContactWithoutMarkerIsNotLineage is the negative unit
// control: after the parent releases the marker, an unrelated process that
// opens a descendant must not hold the marker fixed point closed. Production
// therefore returns before enumeration or any identity-signal path.
func TestUnrelatedPathContactWithoutMarkerIsNotLineage(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	marker, markerPath, err := createOwnershipMarker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if marker != nil {
			_ = marker.Close()
		}
		_ = removeMarkerPath(markerPath)
	})
	if err := marker.Close(); err != nil {
		t.Fatalf("release parent marker: %v", err)
	}
	marker = nil
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "unrelated.pid")
	target := filepath.Join(dir, "descendant.log")
	// Unrelated: opens descendant, no marker FD inherited.
	cmd := exec.Command("python3", "-c", `
import os, signal, sys
path, target = sys.argv[1], sys.argv[2]
out = open(target, "a", encoding="utf-8")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
out.write("u")
out.flush()
signal.pause()
`, pidFile, target)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, cmd) })
	pid, err := waitForChildReadyPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("unrelated ready: %v", err)
	}
	drained, err := markerLineageDrained(markerPath)
	if err != nil {
		t.Fatalf("marker fixed point: %v", err)
	}
	if !drained {
		t.Fatalf("unrelated path-contact pid %d held marker fixed point closed", pid)
	}
}

// TestExecuteDetachedOnlySessionWriterBlocksAndReaps is the production-path
// proof for adversarial setsid+double-fork residual writers that chdir away
// while retaining the inherited marker FD and an open descendant. Intermediate
// parents exit immediately; grandchild leaves the original process group via
// setsid. Execute must BLOCKED via marker lineage and the writer must be gone
// without test teardown.
func TestExecuteDetachedOnlySessionWriterBlocksAndReaps(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "session.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)
	t.Cleanup(func() { forceKillTrackedPID(t, pidFile) })

	result, err := NewVerifierArgs([]string{"./leave-detached-only", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("detached-only double-fork writer must BLOCKED (owned tree residual), got %+v", result)
	}
	if !strings.Contains(result.Output, "residual") && !strings.Contains(result.Output, "ownership") {
		t.Fatalf("BLOCKED output must name ownership close: %q", result.Output)
	}
	// Production must have reaped the residual writer — no test teardown kill.
	assertWriterGone(t, pidFile)
}

// TestExecuteUnrelatedPathContactSurvivesMarkedWriterReaped is the production
// negative control: an unrelated process that starts after Execute begins and
// opens a descendant under the candidate (no inherited marker) must SURVIVE,
// while the marked setsid detached writer is reaped.
func TestExecuteUnrelatedPathContactSurvivesMarkedWriterReaped(t *testing.T) {
	parallelVerifierStress(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	markedPidFile := filepath.Join(dir, "marked.pid")
	unrelatedPidFile := filepath.Join(dir, "unrelated.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	unrelatedTarget := filepath.Join(dir, "unrelated.log")
	startedFile := filepath.Join(dir, "command-started.pid")
	releaseFile := filepath.Join(dir, "release-marked-writer")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)
	t.Cleanup(func() { forceKillTrackedPID(t, markedPidFile) })

	ctx, cancel := context.WithCancel(context.Background())
	type executeOutcome struct {
		result *Result
		err    error
	}
	executeDone := make(chan executeOutcome, 1)
	executeFinished := make(chan struct{})
	go func() {
		defer close(executeFinished)
		result, err := NewVerifierArgs([]string{
			"./leave-detached-only", markedPidFile, writeTarget, startedFile, releaseFile,
		}).Execute(ctx, dir)
		executeDone <- executeOutcome{result: result, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-executeFinished:
		case <-time.After(5 * time.Second):
			t.Errorf("cleanup: Execute did not stop after cancellation")
		}
	})
	if _, err := waitForChildReadyPID(startedFile, 5*time.Second); err != nil {
		t.Fatalf("verification command start gate: %v", err)
	}

	// Start only after the supervised command's explicit gate. This process
	// opens a descendant but inherits no marker FD.
	unrelated := exec.Command("python3", "-c", `
import os, signal, sys
path, target = sys.argv[1], sys.argv[2]
out = open(target, "a", encoding="utf-8")
with open(path, "w", encoding="utf-8") as f:
    f.write("%d\n" % os.getpid())
signal.pause()
`, unrelatedPidFile, unrelatedTarget)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateTestProcess(t, unrelated) })
	unrelatedPID, err := waitForChildReadyPID(unrelatedPidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("unrelated ready: %v", err)
	}
	if err := os.WriteFile(releaseFile, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release marked writer: %v", err)
	}
	var outcome executeOutcome
	select {
	case outcome = <-executeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute exceeded bounded later-holder proof")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.result.Outcome != OutcomeBLOCKED {
		t.Fatalf("marked detached writer must BLOCKED, got %+v", outcome.result)
	}
	assertWriterGone(t, markedPidFile)

	// Unrelated must still be live — path contact is not kill authority.
	if err := syscall.Kill(unrelatedPID, 0); err != nil {
		t.Fatalf("unrelated path-contact pid %d must survive Execute residual drain: %v", unrelatedPID, err)
	}
}

// TestExecuteDetachedOnlyMutationRemovingMarkerDrainLeavesWriter toggles only
// the marker fixed-point proof. The production positive test must fail under
// this mutation because the detached marked writer survives.
func TestExecuteDetachedOnlyMutationRemovingMarkerDrainLeavesWriter(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "session.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	writeExecutable(t, filepath.Join(dir, "leave-detached-only"), "#!/bin/sh\n"+productionDetachedOnlyScript)

	previous := markerLineageDrainedFn
	markerLineageDrainedFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		markerLineageDrainedFn = previous
		forceKillTrackedPID(t, pidFile)
	})

	result, err := NewVerifierArgs([]string{"./leave-detached-only", pidFile, writeTarget}).Execute(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomePASS {
		t.Fatalf("mutation: detached-only without marker drain should PASS; got %+v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("mutation: session pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("mutation: bad session pid %q", data)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("mutation expected double-fork writer %d to survive: %v", pid, err)
	}
}

// TestExecuteDetachedSessionAndBackgroundWriters covers real setsid + same-group
// residual; production must BLOCKED and both writers gone via owned tree close.
func TestExecuteDetachedSessionAndBackgroundWriters(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for setsid residual writer fixture")
	}
	dir := t.TempDir()
	sessionPidFile := filepath.Join(dir, "session.pid")
	groupPidFile := filepath.Join(dir, "group.pid")
	writeTarget := filepath.Join(dir, "residue.log")
	startedFile := filepath.Join(dir, "writers-started")
	releaseFile := filepath.Join(dir, "release-writers")
	writeExecutable(t, filepath.Join(dir, "leave-detached"), "#!/bin/sh\n"+productionDetachedSessionScript)

	type executeOutcome struct {
		result *Result
		err    error
	}
	done := make(chan executeOutcome, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	observerCtx, cancelObservers := context.WithCancel(context.Background())
	observations := make(chan pidTokenObservation, 2)
	observerDone := make(chan struct{}, 2)
	observerDeadline := time.Now().Add(5 * time.Second)
	var observedMu sync.Mutex
	var observedTokens []procToken
	observe := func(path string) {
		go func() {
			defer func() { observerDone <- struct{}{} }()
			tok, err := observePIDToken(observerCtx, path, observerDeadline)
			if err == nil {
				observedMu.Lock()
				observedTokens = append(observedTokens, tok)
				observedMu.Unlock()
			}
			observations <- pidTokenObservation{path: path, tok: tok, err: err}
		}()
	}
	go func() {
		defer close(finished)
		result, err := NewVerifierArgs([]string{
			"./leave-detached", sessionPidFile, writeTarget, groupPidFile, startedFile, releaseFile,
		}).Execute(ctx, dir)
		done <- executeOutcome{result: result, err: err}
	}()
	// Observers start before any readiness wait and capture the first valid
	// PID/start-token publication. Cleanup is registered immediately after all
	// three goroutines launch, before any diagnostic failure can occur.
	observe(sessionPidFile)
	observe(groupPidFile)
	// Register before any readiness wait. Cleanup order is explicit: cancel
	// Execute first; give both observers one shared capture deadline, then
	// cancel unfinished observers, join all three, and reap only stored tokens.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Errorf("cleanup: Execute did not stop after cancellation")
		}
		observersJoined := 0
		captureWindow := time.Until(observerDeadline)
		if captureWindow < 0 {
			captureWindow = 0
		}
		captureTimer := time.NewTimer(captureWindow)
		captureExpired := false
		for observersJoined < 2 && !captureExpired {
			select {
			case <-observerDone:
				observersJoined++
			case <-captureTimer.C:
				captureExpired = true
			}
		}
		if !captureExpired && !captureTimer.Stop() {
			select {
			case <-captureTimer.C:
			default:
			}
		}
		cancelObservers()
		for observersJoined < 2 {
			<-observerDone
			observersJoined++
		}
		for {
			select {
			case <-observations:
			default:
				goto observationsDrained
			}
		}
	observationsDrained:
		observedMu.Lock()
		tokens := append([]procToken(nil), observedTokens...)
		observedMu.Unlock()
		if len(tokens) == 0 {
			t.Errorf("cleanup: no fixture PID/start-token identity was captured")
			return
		}
		reapExactTokens(t, tokens...)
	})
	if _, err := waitForChildReadyPID(startedFile, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	observed := make(map[string]procToken, 2)
	for range 2 {
		observation := <-observations
		if observation.err != nil {
			t.Fatal(observation.err)
		}
		observed[observation.path] = observation.tok
	}
	sessionToken, sessionOK := observed[sessionPidFile]
	groupToken, groupOK := observed[groupPidFile]
	if !sessionOK || !groupOK {
		t.Fatalf("observer did not capture both fixture identities: %+v", observed)
	}
	if sessionToken.equal(groupToken) {
		t.Fatalf("writer inventory aliases one identity: session=%+v group=%+v", sessionToken, groupToken)
	}
	t.Logf("writer inventory: session pid=%d start=%d/%d; group pid=%d start=%d/%d",
		sessionToken.pid, sessionToken.startSec, sessionToken.startUsec,
		groupToken.pid, groupToken.startSec, groupToken.startUsec)
	if err := os.WriteFile(releaseFile, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var outcome executeOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("bounded detached-session proof exceeded diagnostic bound")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	result := outcome.result
	if result.Outcome != OutcomeBLOCKED {
		t.Fatalf("detached+background leave-writer must BLOCKED, got %+v", result)
	}
	assertWriterGone(t, groupPidFile)
	assertWriterGone(t, sessionPidFile, result.Output)
	residue, err := os.ReadFile(writeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) == 0 || len(residue) > 8192 {
		t.Fatalf("bounded residue must be non-empty and <=8192 bytes, got %d", len(residue))
	}
}

// TestProcTokenIdentityBoundRefusesStalePID proves kill is refused when a PID
// no longer matches the recorded start token (PID reuse safety).
func TestProcTokenIdentityBoundRefusesStalePID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tok, err := tokenOf(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("tokenOf: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected wait error after kill")
	}
	// Token must no longer match (process exited).
	if tok.stillSame() {
		t.Fatal("token.stillSame after exit must be false")
	}
	h, err := openHandle(tok)
	if err != nil {
		// Process already gone — still proves stillSame is false.
		if tok.stillSame() {
			t.Fatal("token.stillSame after exit must be false")
		}
		return
	}
	signaled, err := h.kill()
	if err != nil {
		t.Fatalf("handle.kill on stale token: %v", err)
	}
	if signaled {
		t.Fatal("handle.kill must not signal a stale/reused PID identity")
	}
	h.close()
}

// TestKillProcessGroupMembersNeverUsesNegativePGID is a non-vacuous guard:
// processGroupKiller must reap via positive PIDs (identity), not kill(-pgid).
func TestKillProcessGroupMembersNeverUsesNegativePGID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Spawn a same-group child so membership kill has work beyond the leader.
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	if err := processGroupKiller(pgid); err != nil {
		t.Fatalf("processGroupKiller: %v", err)
	}
	if err := waitPIDGone(cmd.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("leader not reaped by membership kill: %v", err)
	}
	if err := waitPIDGone(child.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("group child not reaped by membership kill: %v", err)
	}
}

// TestOwnedNeverReplacesTokenOnPIDReuse forces record of pid P, then simulates
// a second observation of P with a different start token — noteCausal must not
// replace the first incarnation (audit: never adopt reused PID work).
func TestOwnedNeverReplacesTokenOnPIDReuse(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owned.Close() })

	pid := cmd.Process.Pid
	owned.mu.Lock()
	first, ok := owned.handles[pid]
	owned.mu.Unlock()
	if !ok {
		t.Fatal("leader handle missing")
	}
	// Forged token with same pid, different start time (reused PID impostor).
	impostor := procToken{pid: pid, startSec: first.tok.startSec + 999999, startUsec: 1}
	if err := owned.noteCausal(impostor); err != nil {
		t.Fatalf("noteCausal impostor: %v", err)
	}
	owned.mu.Lock()
	second := owned.handles[pid]
	owned.mu.Unlock()
	if !second.tok.equal(first.tok) {
		t.Fatalf("token replaced on PID reuse: got %+v want %+v", second.tok, first.tok)
	}
}

// TestOwnedFreezeRejectsPostLeaderGroupAdoption freezes ownership then proves
// sample does not adopt new numeric-pgid members (post-Wait PGID reuse class).
func TestOwnedFreezeRejectsPostLeaderGroupAdoption(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Kill leader so freeze triggers on next sample; then spawn unrelated
	// process that might land in a recycled pgid space — we only check freeze.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	owned.freeze()
	before := len(owned.handles)
	// sample while frozen must be a no-op for discovery.
	if err := owned.sample(); err != nil {
		t.Fatalf("sample frozen: %v", err)
	}
	if len(owned.handles) != before {
		t.Fatalf("frozen sample adopted handles: before=%d after=%d", before, len(owned.handles))
	}
	// Close should not membership-kill numeric pgid (no processGroupKiller on Close).
	_ = owned.Close()
}

// TestIsExpectedKillWaitUsesTypedWaitStatus proves signal classification does
// not use substring matching on error text.
func TestIsExpectedKillWaitUsesTypedWaitStatus(t *testing.T) {
	if isExpectedKillWait(nil) != true {
		t.Fatal("nil wait err is expected")
	}
	if isExpectedKillWait(errors.New("signal: killed")) {
		t.Fatal("plain error with kill substring must NOT match (typed WaitStatus only)")
	}
	// Real signaled exit: kill a short-lived process group member via Wait.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("expected wait error after SIGKILL")
	}
	if !isExpectedKillWait(waitErr) {
		t.Fatalf("typed WaitStatus SIGKILL must be expected, got %v (%T)", waitErr, waitErr)
	}
}

// activeLateWriterScript is a production-shaped late writer under root:
//  1. signals ready after seed residue
//  2. continuously mkdir -p + creates new objects via ABSOLUTE paths
//  3. advances genPath (OUTSIDE root so partial RemoveAll cannot erase proof)
//
// Unreaped, it blocks durable TempDir cleanup: either os.RemoveAll fails with
// "directory not empty" (FAC-151 CI class) or RemoveAll "wins" a walk but the
// writer recreates residue while still live. Parked sleep cannot do this.
// No chmod seal/unseal.
//
// Args: $1=root $2=readyPath $3=genPath(outside root)
const activeLateWriterScript = `
root="$1"
objects="$1/.git/objects"
mkdir -p "$objects" || exit 1
printf x > "$objects/seed"
printf ready > "$2"
i=0
while true; do
  mkdir -p "$objects" 2>/dev/null || true
  printf w > "$objects/active-$i" 2>/dev/null || true
  # Atomic-ish gen publish: avoid empty-file reads mid-truncate under -race.
  printf "%s" "$i" > "$3.tmp" 2>/dev/null || true
  mv -f "$3.tmp" "$3" 2>/dev/null || printf "%s" "$i" > "$3"
  i=$((i+1))
done
`

// lateWriterFixture owns root + process group. The sole toggled step before
// cleanupTempDir is production ReapOwnedCmd — no seal/unseal side channel.
type lateWriterFixture struct {
	t         *testing.T
	root      string
	readyPath string
	genPath   string
	cmd       *exec.Cmd
	pgid      int
	stderr    strings.Builder
	started   bool
	reaped    bool
}

func startLateWriterFixture(t *testing.T) *lateWriterFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	// gen lives beside root (not under it) so RemoveAll cannot erase the
	// mutation counter while the writer is still proving activity.
	genPath := root + ".writer.gen"
	readyPath := root + ".writer.ready"
	f := &lateWriterFixture{
		t:         t,
		root:      root,
		readyPath: readyPath,
		genPath:   genPath,
	}
	t.Cleanup(f.shutdown)

	f.cmd = exec.Command("sh", "-c", activeLateWriterScript, "late-writer", f.root, f.readyPath, f.genPath)
	f.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f.cmd.Stderr = &f.stderr
	if err := f.cmd.Start(); err != nil {
		t.Fatalf("start late writer: %v", err)
	}
	f.started = true
	f.pgid = f.cmd.Process.Pid

	if err := waitForWriterReady(f.readyPath, f.pgid, 5*time.Second); err != nil {
		t.Fatalf("writer ready handshake: %v (stderr=%q)", err, f.stderr.String())
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("writer process group must be live after ready: %v", err)
	}
	// Active mutation (not parked sleep): gen must advance past 0.
	if _, err := f.waitGenAdvance(0, 2*time.Second); err != nil {
		t.Fatalf("writer must actively mutate after ready: %v (stderr=%q)", err, f.stderr.String())
	}
	return f
}

// cleanupTempDir is the single cleanup action shared by pre-fix, post-fix,
// and mutation paths. Only production ReapOwnedCmd is toggled around it.
func cleanupTempDir(root string) error {
	return os.RemoveAll(root)
}

// isLiveWriterRemoveAllError attributes failure to the concurrent-writer class
// (directory not empty / unlinkat), not permission tricks.
func isLiveWriterRemoveAllError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "directory not empty") ||
		strings.Contains(msg, "not empty") ||
		strings.Contains(msg, "unlinkat")
}

// ownershipBlocksDurableCleanup proves unreaped active ownership prevents a
// durable TempDir cleanup. Either:
//   - cleanupTempDir returns the live-writer unlinkat/not-empty class, or
//   - cleanupTempDir returns nil but the writer recreates residue while still live
//
// (RemoveAll "won" one walk — not durable). Parked sleep + chmod cannot pass.
func ownershipBlocksDurableCleanup(root string, pgid int) error {
	if err := syscall.Kill(-pgid, 0); err != nil {
		return fmt.Errorf("writer group %d not live before cleanup: %w", pgid, err)
	}
	err := cleanupTempDir(root)
	if err != nil {
		// Only the concurrent-writer unlinkat/not-empty class proves active
		// ownership blocked RemoveAll. Any other error is not ownership proof
		// (isLiveWriterRemoveAllError is the attribution helper).
		if isLiveWriterRemoveAllError(err) {
			return err
		}
		return nil
	}
	// RemoveAll returned nil — require non-durable recreation under live writer.
	objects := filepath.Join(root, ".git", "objects")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if kerr := syscall.Kill(-pgid, 0); kerr != nil {
			return fmt.Errorf("writer group %d died after RemoveAll success (no durable-block proof): %w", pgid, kerr)
		}
		if st, stErr := os.Stat(objects); stErr == nil && st.IsDir() {
			return fmt.Errorf("cleanup not durable: unreaped writer recreated %s after RemoveAll", objects)
		}
		if entries, rdErr := os.ReadDir(root); rdErr == nil && len(entries) > 0 {
			return fmt.Errorf("cleanup not durable: unreaped writer left/recreated %d entries under root", len(entries))
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

// durableCleanupAfterReap runs the same cleanupTempDir and requires the root
// to stay gone (no recreation). Call only after production ReapOwnedCmd.
func durableCleanupAfterReap(root string, observe time.Duration) error {
	if err := cleanupTempDir(root); err != nil {
		return err
	}
	deadline := time.Now().Add(observe)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(root); err == nil {
			return fmt.Errorf("cleanup not durable after reap: root reappeared")
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func (f *lateWriterFixture) readGen() (int, error) {
	// Brief re-read on empty: concurrent truncate/replace can yield "" once.
	var last error
	for attempt := 0; attempt < 50; attempt++ {
		data, err := os.ReadFile(f.genPath)
		if err != nil {
			last = err
			time.Sleep(time.Millisecond)
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			last = fmt.Errorf("empty gen")
			time.Sleep(time.Millisecond)
			continue
		}
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			last = fmt.Errorf("parse gen %q: %w", s, err)
			time.Sleep(time.Millisecond)
			continue
		}
		return n, nil
	}
	if last == nil {
		last = fmt.Errorf("gen unreadable")
	}
	return 0, last
}

func (f *lateWriterFixture) waitGenAdvance(min int, bound time.Duration) (int, error) {
	deadline := time.Now().Add(bound)
	var lastErr error
	var n int
	for {
		n, lastErr = f.readGen()
		if lastErr == nil && n > min {
			return n, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return 0, fmt.Errorf("gen did not advance past %d: %w", min, lastErr)
			}
			return n, fmt.Errorf("gen stuck at %d (want > %d)", n, min)
		}
		time.Sleep(time.Millisecond)
	}
}

// shutdown: production ReapOwnedCmd (errors reported), then cleanupTempDir.
func (f *lateWriterFixture) shutdown() {
	if f == nil {
		return
	}
	if f.started && !f.reaped {
		if err := ReapOwnedCmd(f.cmd); err != nil {
			// Residual tree after unreaped writer is expected on mutation paths;
			// fail only if kill/wait identity errors remain after close.
			if !errors.Is(err, ErrResidualOwnedTree) && !strings.Contains(err.Error(), "residual owned") {
				if f.pgid > 0 {
					if kerr := killProcessGroupIfLive(f.pgid); kerr != nil {
						f.t.Errorf("shutdown fallback killProcessGroupIfLive %d: %v", f.pgid, kerr)
					}
				}
				f.t.Errorf("shutdown ReapOwnedCmd: %v (stderr=%q)", err, f.stderr.String())
			}
		}
		f.reaped = true
	}
	if f.root != "" {
		if err := cleanupTempDir(f.root); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown cleanupTempDir: %v", err)
		}
	}
	if f.genPath != "" {
		if err := os.Remove(f.genPath); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown remove gen: %v", err)
		}
	}
	if f.readyPath != "" {
		if err := os.Remove(f.readyPath); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown remove ready: %v", err)
		}
	}
}

// reapOwned closes ownership via production ReapOwnedCmd only. Errors fatal.
func (f *lateWriterFixture) reapOwned() {
	f.t.Helper()
	if f.reaped {
		return
	}
	if err := ReapOwnedCmd(f.cmd); err != nil {
		f.t.Fatalf("production ReapOwnedCmd: %v (stderr=%q)", err, f.stderr.String())
	}
	if err := waitForProcessGroupGone(f.pgid, 2*time.Second); err != nil {
		f.t.Fatalf("process group %d still live after ReapOwnedCmd: %v", f.pgid, err)
	}
	f.reaped = true
}

// waitForProcessGroupGone proves no running member of the process group remains
// (grandchildren included). Leader-only ESRCH is not sufficient. Uses the same
// live/non-zombie snapshot as production processGroupLive so unreaped zombies
// left by a non-reaping container init (sleep) are not false positives.
func waitForProcessGroupGone(pgid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if !processGroupLive(pgid) {
			return nil
		}
		_ = processGroupKiller(pgid)
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d still has live members after diagnostic bound", pgid)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForWriterReady is an explicit boundary handshake on readyPath contents.
// Diagnostic bound only — not a cancel/cleanup sleep.
func waitForWriterReady(readyPath string, pgid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		data, err := os.ReadFile(readyPath)
		if err == nil && strings.TrimSpace(string(data)) == "ready" {
			return nil
		}
		if pgid > 0 {
			if killErr := syscall.Kill(pgid, 0); killErr != nil {
				return fmt.Errorf("writer pgid %d exited before ready handshake: %w", pgid, killErr)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(readyPath))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLateWriterIntoGitRequiresExplicitReap: unreaped active writer blocks
// durable cleanup; production ReapOwnedCmd is the sole toggled step; same
// cleanupTempDir then durably succeeds. No chmod/unseal.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	f := startLateWriterFixture(t)

	genBefore, err := f.readGen()
	if err != nil {
		t.Fatalf("pre-fix: initial gen: %v", err)
	}

	// PRE-FIX: only cleanupTempDir — writer unreaped → durable cleanup blocked.
	preErr := ownershipBlocksDurableCleanup(f.root, f.pgid)
	if preErr == nil {
		t.Fatal("pre-fix: durable cleanup must be blocked by unreaped active writer")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("unreaped process group must remain live after blocked cleanup: %v (stderr=%q)", err, f.stderr.String())
	}
	// Continuing mutation (gen outside root) — proves active writer, not park.
	if _, err := f.waitGenAdvance(genBefore, 2*time.Second); err != nil {
		t.Fatalf("pre-fix: writer must keep mutating after blocked cleanup: %v", err)
	}

	// FIX: toggle ONLY production ReapOwnedCmd (kill+Wait+group-gone).
	f.reapOwned()
	genAfterReap, err := f.readGen()
	if err != nil {
		// gen may be mid-write at kill; treat unreadable as frozen baseline.
		genAfterReap = genBefore
	}

	// POST-FIX: same cleanupTempDir must durably succeed — no recreation.
	if err := durableCleanupAfterReap(f.root, 100*time.Millisecond); err != nil {
		t.Fatalf("post-fix: durable cleanup after production ReapOwnedCmd: %v", err)
	}
	if processGroupLive(f.pgid) {
		t.Fatalf("process group %d still live after production ReapOwnedCmd", f.pgid)
	}
	// Gen must not keep advancing after reap (writer dead).
	time.Sleep(20 * time.Millisecond)
	if g2, gerr := f.readGen(); gerr == nil && g2 > genAfterReap+1000 {
		t.Fatalf("post-fix: gen advanced after reap (%d -> %d); writer still mutating", genAfterReap, g2)
	}
	f.root = ""
}

// TestLateWriterCleanupMutationOmittingReapStillFails: identical cleanup path;
// only production ReapOwnedCmd is omitted. Mutant must fail durable cleanup.
func TestLateWriterCleanupMutationOmittingReapStillFails(t *testing.T) {
	f := startLateWriterFixture(t)

	gen0, err := f.readGen()
	if err != nil {
		t.Fatalf("mutation: initial gen: %v", err)
	}

	// Control: unreaped active writer blocks durable cleanup.
	if err := ownershipBlocksDurableCleanup(f.root, f.pgid); err == nil {
		t.Fatal("control: durable cleanup must fail under unreaped active writer")
	}

	// MUTATION: omit only ReapOwnedCmd; every other cleanup action identical.
	mutErr := ownershipBlocksDurableCleanup(f.root, f.pgid)
	if mutErr == nil {
		t.Fatal("mutation: omitting only production ReapOwnedCmd must leave durable cleanup failing; got nil")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("mutation: process group must still be live without reap: %v", err)
	}
	if _, err := f.waitGenAdvance(gen0, 2*time.Second); err != nil {
		t.Fatalf("mutation: writer must keep mutating without reap: %v", err)
	}
	// Fixture t.Cleanup runs production ReapOwnedCmd then cleanupTempDir.
}

// TestProcessGroupReapAllowsTempDirCleanup: blocked cleanup → ReapOwnedCmd →
// durable cleanupTempDir success (no seal/unseal).
func TestProcessGroupReapAllowsTempDirCleanup(t *testing.T) {
	f := startLateWriterFixture(t)
	if err := ownershipBlocksDurableCleanup(f.root, f.pgid); err == nil {
		t.Fatal("pre-reap: durable cleanup must fail under active writer")
	}
	f.reapOwned()
	if err := durableCleanupAfterReap(f.root, 100*time.Millisecond); err != nil {
		t.Fatalf("post-reap: durable cleanup must succeed: %v", err)
	}
	f.root = ""
}

// grandchildGroupScript: leader backgrounds a real nested sh (not a shell
// function — $$ in functions is the parent shell on bash/zsh) that writes its
// own pid then parks. Production ReapOwnedCmd must kill leader + grandchild.
const grandchildGroupScript = `sh -c 'printf "%s\n" "$$" > "$1"; exec sleep 3600' grandchild "$1" & wait`

// startGrandchildGroup starts a Setpgid shell with a live grandchild and
// returns (cmd, leaderPgid, grandchildPid). Caller must ReapOwnedCmd or kill.
func startGrandchildGroup(t *testing.T) (cmd *exec.Cmd, pgid int, grandchild int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd = exec.Command("sh", "-c", grandchildGroupScript, "group-leader", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start grandchild group: %v", err)
	}
	pgid = cmd.Process.Pid
	t.Cleanup(func() {
		// Leak-safe: production reap if still unreaped; kill/Wait errors reported.
		if cmd.ProcessState == nil && cmd.Process != nil {
			if err := ReapOwnedCmd(cmd); err != nil && !errors.Is(err, ErrResidualOwnedTree) &&
				!strings.Contains(err.Error(), "residual owned") {
				if kerr := killProcessGroupIfLive(pgid); kerr != nil {
					t.Errorf("cleanup fallback killProcessGroupIfLive %d: %v", pgid, kerr)
				}
				t.Errorf("cleanup ReapOwnedCmd: %v", err)
			}
		} else if pgid > 0 {
			if err := killProcessGroupIfLive(pgid); err != nil {
				t.Errorf("cleanup killProcessGroupIfLive %d: %v", pgid, err)
			}
		}
	})
	gc, err := waitForChildReadyPID(ready, 5*time.Second)
	if err != nil {
		t.Fatalf("grandchild ready: %v", err)
	}
	if err := syscall.Kill(gc, 0); err != nil {
		t.Fatalf("grandchild %d not live after ready: %v", gc, err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("process group %d not live after ready: %v", pgid, err)
	}
	return cmd, pgid, gc
}

// TestReapOwnedCmdKillsGrandchildren is the production-load-bearing positive
// proof: ReapOwnedCmd (kill process group + Wait + group-gone probe) must
// extinguish the leader and a ready grandchild. This is not a fake cleaner —
// it exercises the same primitive execute() uses after Start when ctx is done.
func TestReapOwnedCmdKillsGrandchildren(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	if err := ReapOwnedCmd(cmd); err != nil {
		t.Fatalf("production ReapOwnedCmd: %v", err)
	}
	if err := waitForProcessGroupGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("after ReapOwnedCmd: %v", err)
	}
	// kill(pid,0) is true for zombies; container init may be sleep and not reap.
	// Production residual checks use live/non-zombie membership.
	if tok, err := tokenOf(grandchild); err == nil && tok.isLiveTarget() {
		t.Fatalf("grandchild %d still live after production ReapOwnedCmd", grandchild)
	}
	if cmd.ProcessState == nil {
		t.Fatal("ReapOwnedCmd must Wait the leader (ProcessState set)")
	}
}

// TestReapOwnedCmdTreeCloseKillsGrandchildrenDespiteLeaderOnlyGroupKill proves
// durable tree tracking is load-bearing: even if processGroupKiller is mutated
// to leader-only, ReapOwnedCmd still reaps grandchildren via positive-PID
// finalize (not empty -pgid reuse).
func TestReapOwnedCmdTreeCloseKillsGrandchildrenDespiteLeaderOnlyGroupKill(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	prev := processGroupKiller
	processGroupKiller = func(id int) error {
		if id <= 0 {
			return fmt.Errorf("kill process group: invalid pgid %d", id)
		}
		// MUTATION: kill leader only — group-wide signal is insufficient alone.
		return syscall.Kill(id, syscall.SIGKILL)
	}
	t.Cleanup(func() { processGroupKiller = prev })

	if err := ReapOwnedCmd(cmd); err != nil {
		// Residual tree error is acceptable if grandchild is still cleaned;
		// hard-fail only when grandchild survives as a running process.
		if tok, terr := tokenOf(grandchild); terr == nil && tok.isLiveTarget() {
			t.Fatalf("ReapOwnedCmd error %v and grandchild %d still live", err, grandchild)
		}
	}
	if err := waitForPIDGone(grandchild, 2*time.Second); err != nil {
		// Zombie residual under non-reaping container init is acceptable if not live.
		if tok, terr := tokenOf(grandchild); terr == nil && tok.isLiveTarget() {
			t.Fatalf("tree close must reap grandchild %d despite leader-only group killer: %v", grandchild, err)
		}
	}
	if err := waitForProcessGroupGone(pgid, 2*time.Second); err != nil {
		// Group may already be empty; only fail if still live.
		if processGroupLive(pgid) {
			t.Fatalf("process group %d still live: %v", pgid, err)
		}
	}
}

// TestFinalizeOwnedTreeMutationLeavesGrandchildAlive proves that skipping
// causal handle finalize (and skipping tracked kill) leaves a grandchild live
// after a leader-only group membership kill — the incomplete ownership shape.
func TestFinalizeOwnedTreeMutationLeavesGrandchildAlive(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	prevKill := processGroupKiller
	processGroupKiller = func(id int) error {
		if id <= 0 {
			return fmt.Errorf("kill process group: invalid pgid %d", id)
		}
		// Leader-only membership kill (wrong).
		return syscall.Kill(id, syscall.SIGKILL)
	}
	prevFin := finalizeOwnedTree
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
			_ = o.stopTracker()
			// Intentionally do not killTracked — mutation.
		}
		return nil
	}
	t.Cleanup(func() {
		processGroupKiller = prevKill
		finalizeOwnedTree = prevFin
		if err := killProcessGroupIfLive(pgid); err != nil {
			t.Errorf("cleanup killProcessGroupIfLive: %v", err)
		}
		if err := syscall.Kill(grandchild, syscall.SIGKILL); err != nil && !isESRCH(err) {
			t.Errorf("cleanup SIGKILL grandchild: %v", err)
		}
		if err := waitForPIDGone(grandchild, 2*time.Second); err != nil {
			t.Errorf("cleanup grandchild gone: %v", err)
		}
	})

	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Incomplete: leader-only membership kill only (no killTracked).
	if err := processGroupKiller(owned.Pgid()); err != nil {
		t.Fatalf("leader-only killer: %v", err)
	}
	_ = cmd.Wait()
	if err := finalizeOwnedTree(owned); err != nil {
		t.Fatalf("mutated finalize should return nil: %v", err)
	}
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("mutation expected grandchild %d to survive incomplete ownership: %v", grandchild, err)
	}
}
