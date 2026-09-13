package mergeadmit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The squash-range proof's ancestry gate is a subprocess like every other git
// probe in this package, and it must live under the same owned-process
// discipline: procsignal.CommandContext puts the child in an owned process
// group so the caller's deadline kills the child AND its descendants. The
// pre-fix path rode harvest.IsAncestor, whose plain exec.CommandContext kills
// only the direct child — a slow merge-base --is-ancestor left its process
// tree alive after the caller's deadline had fired, and swallowed the deadline
// into a "not an ancestor" evidence answer, the exact
// cancellation-reads-as-no-evidence classification the drain contract forbids.
//
// This regression drives the REAL production path (ProveEquivalentLandedContext
// → matchSquashRangeReplay → ancestry probe) against a PATH-shim git whose
// merge-base --is-ancestor invocation blocks behind a real descendant process
// while the parent deadline fires. It proves, causally:
//
//  1. the ancestry probe is actually reached and actually blocks;
//  2. the recorded descendant PID is ALIVE while the parent is blocked, before
//     the deadline;
//  3. the proof returns the bare caller deadline (errors.Is
//     context.DeadlineExceeded), never a flat evidence refusal;
//  4. after the return, every owned descendant is gone (the process group was
//     killed, not just the direct child);
//  5. no further proof subprocess spawns after the cancellation.
func TestSquashAncestryProbeBlockedDeadlineKillsOwnedGroup(t *testing.T) {
	// Resolve the real git BEFORE the shim directory shadows PATH.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	// True squash lifecycle, the exact shape the positive squash test admits:
	// two reviewed commits, squash-landed onto main, main advancing after.
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "base\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "a.txt", "intermediate\n", "first reviewed edit")
	candidate := commit(t, dir, "a.txt", "final\n", "second reviewed edit")
	run(t, dir, "git", "checkout", "-q", "main")
	commit(t, dir, "before.txt", "unrelated\n", "main advance")
	commit(t, dir, "a.txt", "final\n", "squashed range")
	landed := commit(t, dir, "after.txt", "later\n", "later main")

	// The shim: every git call delegates to the real binary; only
	// merge-base --is-ancestor — the ancestry probe under correction —
	// records its invocation, spawns a long-lived descendant, and blocks
	// until something kills it.
	shimDir := t.TempDir()
	counter := filepath.Join(shimDir, "invocations")
	pids := filepath.Join(shimDir, "pids")
	shim := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "merge-base" ] && [ "$2" = "--is-ancestor" ]; then
  echo x >> "%s"
  sleep 300 &
  echo $! >> "%s"
  echo $$ >> "%s"
  while :; do sleep 1; done
fi
exec "%s" "$@"
`, counter, pids, pids, realGit)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var proofResult error

	// Every process the shim records is a test-owned process; whatever the
	// assertions find, cleanup reaps them so no orphan outlives the test.
	t.Cleanup(func() {
		cancel()
		data, err := os.ReadFile(pids)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	proofErr := make(chan error, 1)
	go func() {
		_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
			BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed,
		})
		proofErr <- err
	}()

	// (1) The probe is reached and blocks: its descendant appears and is
	// (2) alive while the parent is still inside its deadline.
	var sleepPID int
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pid, ok := firstIntTag(t, pids); ok {
			sleepPID = pid
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture precondition failed: the ancestry probe never spawned")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(sleepPID, 0); err != nil {
		t.Fatalf("fixture precondition failed: recorded descendant %d is not alive while the probe is blocked: %v", sleepPID, err)
	}

	// The parent deadline fires; wait for the proof to come back before
	// inspecting what it left behind.
	select {
	case err := <-proofErr:
		proofResult = err
	case <-time.After(30 * time.Second):
		t.Fatal("proof did not return after its deadline fired")
	}

	// (4) The owned group is dead: the recorded descendant is gone and stays
	// gone. The pre-fix path kills only the direct child, so the descendant
	// outlives the deadline and this poll exhausts.
	gone := false
	deadline = time.Now().Add(15 * time.Second)
	for {
		if err := syscall.Kill(sleepPID, 0); err != nil {
			gone = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		syscall.Kill(sleepPID, syscall.SIGKILL)
		t.Fatalf("descendant %d of the ancestry probe survived the caller's deadline: the probe is not owned-process-group cleanup", sleepPID)
	}
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(sleepPID, 0); err == nil {
		syscall.Kill(sleepPID, syscall.SIGKILL)
		t.Fatalf("descendant %d reappeared after cleanup", sleepPID)
	}

	// (3) The proof is classified as the caller's deadline, never flattened
	// into an evidence refusal the drain would read as "not landed".
	if !errors.Is(proofResult, context.DeadlineExceeded) {
		t.Fatalf("a fired deadline must return as the bare context error, got: %v", proofResult)
	}

	// (5) No later proof spawns: after the cancelled proof returned, the
	// shim's invocation log is static — a deadline that read as "not an
	// ancestor" would fall through and keep probing.
	spawnsAtReturn := shimCount(t, counter)
	time.Sleep(1500 * time.Millisecond)
	if after := shimCount(t, counter); after != spawnsAtReturn {
		t.Fatalf("the cancelled proof kept spawning probes: %d at return, %d afterwards", spawnsAtReturn, after)
	}

	// The probe ran exactly once: blocked, cancelled, never re-probed.
	if spawnsAtReturn != 1 {
		t.Fatalf("expected exactly one blocked ancestry probe, got %d", spawnsAtReturn)
	}
}

// TestSquashAncestryRefusalsKeepTheirMeaning pins the fail-closed ancestry
// semantics the owned probe must preserve: a proven negative (exit 1) stays an
// evidence refusal, and the positive squash case still proves.
func TestSquashAncestryRefusalsKeepTheirMeaning(t *testing.T) {
	dir := gitRepo(t)
	base := commit(t, dir, "a.txt", "base\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	commit(t, dir, "a.txt", "intermediate\n", "first reviewed edit")
	candidate := commit(t, dir, "a.txt", "final\n", "second reviewed edit")
	run(t, dir, "git", "checkout", "-q", "main")
	commit(t, dir, "before.txt", "unrelated\n", "main advance")
	// The squash endpoint's content diverges from the reviewed range: ancestry
	// of the base holds, but the replay refuses. The negative must stay a
	// refusal with the ancestry gate never flattening into it.
	landed := commit(t, dir, "a.txt", "altered\n", "squashed range")
	if _, err := ProveEquivalentLanded(dir, ProofRequest{BaseSHA: base, CandidateSHA: candidate, LandedSHA: landed}); err == nil {
		t.Fatal("altered squash must stay refused")
	}

	// A reviewed range whose base is not an ancestor of the candidate at all:
	// the ancestry gate's own proven negative. The orphan root shares no
	// history with the reviewed work.
	run(t, dir, "git", "checkout", "-q", "--orphan", "orphan")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "orphan root")
	orphan := revParse(t, dir, "HEAD")
	if _, _, err := matchSquashRangeReplay(context.Background(), dir, orphan, candidate, []string{landed}); err == nil || !strings.Contains(err.Error(), "ancestry is unproven") {
		t.Fatalf("an unproven ancestry must be refused as evidence, got: %v", err)
	}
}

// firstIntTag reads the first integer line of the file, if any.
func firstIntTag(t *testing.T, path string) (int, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("shim recorded a non-integer pid %q: %v", line, err)
		}
		return pid, true
	}
	return 0, false
}

// shimCount counts the merge-base --is-ancestor invocations the shim logged.
func shimCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}
