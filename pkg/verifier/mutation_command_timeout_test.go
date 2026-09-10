package verifier

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/laneenv"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/slot"
)

func isolateOneTestSlot(t *testing.T) {
	t.Helper()
	requireHeldMarkerAbsent(t)
	restore, err := laneenv.IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	t.Setenv(slot.EnvCount, strconv.Itoa(slot.DefaultCount))
}

func requireHeldMarkerAbsent(t *testing.T) {
	t.Helper()
	if err := os.Unsetenv(slot.EnvHeld); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(slot.EnvHeld) == "1" {
		t.Fatal("HERD_HEAVY_PHASE_SLOT_HELD=1 would make a real acquire a no-op")
	}
}

func requireIsolatedSlotEmpty(t *testing.T) {
	t.Helper()
	heavy, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	if holders := heavy.Status(); len(holders) != 0 {
		t.Fatalf("isolated slot still held: %+v", holders)
	}
}

func mutantStartArgv(marker string) []string {
	return []string{"sh", "-c", `if [ "$(cat candidate.txt)" = "mutant" ]; then printf 'started\n' > "$1"; sleep 30; fi`, "mutant-start", marker}
}

func commandStartArgv(marker string) []string {
	return []string{"sh", "-c", `printf 'started\n' > "$1"`, "command-start", marker}
}

func mutationOutcomeArgv(marker string) []string {
	return []string{"sh", "-c", `IFS= read -r candidate < candidate.txt; if [ "$candidate" = "mutant" ]; then printf 'mutant\n' >> "$1"; exit 1; fi; printf 'original\n' >> "$1"; exit 0`, "mutation-outcome", marker}
}

func waitBaselineThenHoldSlot(t *testing.T, baselineDone, slotHeld chan struct{}) *slot.Lease {
	t.Helper()
	var once sync.Once
	signalHeld := func() { once.Do(func() { close(slotHeld) }) }
	t.Cleanup(signalHeld)

	select {
	case <-baselineDone:
	case <-time.After(10 * time.Second):
		t.Fatal("baseline did not finish before mutant slot acquisition")
	}

	heavy, err := slot.Default()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := heavy.Acquire(context.Background(), "test-hold-isolated-slot", time.Second)
	if err != nil {
		t.Fatalf("test failed to occupy isolated slot: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	signalHeld()
	return lease
}

func TestRunMutationCheck_QueuedVacuousMutantFailsAfterSlotWait(t *testing.T) {
	isolateOneTestSlot(t)
	dir, candidate := mutationRepo(t, false)
	commandTimeout := 100 * time.Millisecond

	baselineDone := make(chan struct{})
	slotHeld := make(chan struct{})
	v := NewVerifierArgs([]string{"true"})
	v.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true}
	})
	v.afterMutationApplied = func() {
		close(baselineDone)
		<-slotHeld
	}

	type outcome struct {
		result *MutationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
			CandidateSHA:      candidate,
			EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile:        "candidate.txt",
			OriginalCode:      "original\n",
			MutantCode:        "mutant\n",
			Timeout:           commandTimeout,
		})
		done <- outcome{result: result, err: err}
	}()

	lease := waitBaselineThenHoldSlot(t, baselineDone, slotHeld)
	if err := lease.Release(); err != nil {
		t.Fatalf("release isolated slot: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("vacuous mutation after slot wait should return a FAIL result: %v", got.err)
	}
	result := got.result
	if result == nil {
		t.Fatal("expected mutation result")
	}
	if result.Outcome != OutcomeFAIL || result.Killed || !result.Restored {
		t.Fatalf("queued vacuous mutant must FAIL and restore, not consume the command timeout while queued: %+v", result)
	}
	if result.Mutant.Duration <= 0 {
		t.Fatalf("mutant command must run after the slot is released: duration=%v output=%q", result.Mutant.Duration, result.Output)
	}
	if result.Outcome == OutcomeBLOCKED || strings.Contains(result.Output, "context deadline exceeded") {
		t.Fatalf("slot wait must not classify as command timeout: %+v", result)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	requireIsolatedSlotEmpty(t)
}

func TestRunMutationCheck_CommandDeadlineExcludesPreparation(t *testing.T) {
	isolateOneTestSlot(t)
	dir, candidate := mutationRepo(t, false)
	marker := filepath.Join(t.TempDir(), "mutation-outcome")
	ready := make(chan struct{})
	release := make(chan struct{})
	var mutant atomic.Bool
	var gate sync.Once
	var timerArmed atomic.Bool
	var preparationActive atomic.Bool
	preparationActive.Store(true)

	v := NewVerifierArgs(mutationOutcomeArgv(marker))
	v.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true}
	})
	v.afterMutationApplied = func() { mutant.Store(true) }
	v.afterFunc = func(d time.Duration, f func()) *time.Timer {
		if mutant.Load() {
			timerArmed.Store(true)
			if preparationActive.Load() {
				t.Errorf("command timer armed while preparation was still active")
			}
		}
		return time.AfterFunc(d, f)
	}
	v.beforeCommandStart = func(ctx context.Context) {
		if !mutant.Load() {
			return
		}
		gate.Do(func() {
			if timerArmed.Load() {
				t.Errorf("command timer was armed before beforeCommandStart")
			}
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Errorf("commandCtx has deadline during preparation: preparation must exclude command deadline")
			}
			close(ready)
			<-release
			preparationActive.Store(false)
		})
	}

	done := make(chan *MutationResult, 1)
	go func() {
		result, _ := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
			CandidateSHA: candidate, EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile: "candidate.txt", OriginalCode: "original\n", MutantCode: "mutant\n",
			Timeout: 2 * time.Second,
		})
		done <- result
	}()
	<-ready
	time.Sleep(200 * time.Millisecond)
	close(release)
	result := <-done
	if result == nil || result.Outcome != OutcomePASS || !result.Killed || !result.Restored {
		t.Fatalf("command deadline must exclude preparation: %+v", result)
	}
	if !timerArmed.Load() {
		t.Fatalf("expected command timer to be armed during mutant execution")
	}
	if result.Baseline.ExitCode != 0 || result.Mutant.ExitCode != 1 || result.Final.ExitCode != 0 {
		t.Fatalf("expected 0/1/0 exits: baseline=%d mutant=%d final=%d", result.Baseline.ExitCode, result.Mutant.ExitCode, result.Final.ExitCode)
	}
	markerBytes, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(markerBytes), "mutant\n") || !strings.HasSuffix(string(markerBytes), "original\n") {
		t.Fatalf("marker must record mutant execution and restored original execution: %q (err=%v)", markerBytes, err)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	requireIsolatedSlotEmpty(t)
}

func TestRunMutationCheck_ParentCancelWhileQueuedStartsNoChild(t *testing.T) {
	isolateOneTestSlot(t)
	dir, candidate := mutationRepo(t, false)
	marker := filepath.Join(t.TempDir(), "mutant-started")
	_ = os.Remove(marker)

	baselineDone := make(chan struct{})
	slotHeld := make(chan struct{})
	v := NewVerifierArgs(mutantStartArgv(marker))
	v.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true}
	})
	v.afterMutationApplied = func() {
		_ = os.Remove(marker)
		close(baselineDone)
		<-slotHeld
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result *MutationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := v.RunMutationCheckForCandidate(ctx, dir, MutationRequest{
			CandidateSHA:      candidate,
			EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile:        "candidate.txt",
			OriginalCode:      "original\n",
			MutantCode:        "mutant\n",
			Timeout:           2 * time.Second,
		})
		done <- outcome{result: result, err: err}
	}()

	lease := waitBaselineThenHoldSlot(t, baselineDone, slotHeld)
	time.Sleep(50 * time.Millisecond)
	cancel()

	got := <-done
	if err := lease.Release(); err != nil {
		t.Fatalf("release isolated slot: %v", err)
	}
	if got.err != nil {
		t.Fatalf("parent cancel while queued should return a BLOCKED result: %v", got.err)
	}
	result := got.result
	if result == nil {
		t.Fatal("expected mutation result")
	}
	if result.Outcome != OutcomeBLOCKED || result.Killed || !result.Restored {
		t.Fatalf("parent cancel while queued must block and restore: %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled queued mutant must not start a child; marker state err=%v", err)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	requireIsolatedSlotEmpty(t)
}

func TestRunMutationCheck_SlotWaitExhaustionStartsNoCommand(t *testing.T) {
	isolateOneTestSlot(t)
	t.Setenv(slot.EnvTimeout, "80ms")
	dir, candidate := mutationRepo(t, false)
	marker := filepath.Join(t.TempDir(), "command-started")
	_ = os.Remove(marker)

	baselineDone := make(chan struct{})
	slotHeld := make(chan struct{})
	v := NewVerifierArgs(commandStartArgv(marker))
	v.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		return resources.DiskDecision{Allowed: true}
	})
	v.afterMutationApplied = func() {
		_ = os.Remove(marker)
		close(baselineDone)
		<-slotHeld
	}

	type outcome struct {
		result *MutationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
			CandidateSHA:      candidate,
			EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile:        "candidate.txt",
			OriginalCode:      "original\n",
			MutantCode:        "mutant\n",
			Timeout:           2 * time.Second,
		})
		done <- outcome{result: result, err: err}
	}()

	lease := waitBaselineThenHoldSlot(t, baselineDone, slotHeld)
	got := <-done
	if err := lease.Release(); err != nil {
		t.Fatalf("release isolated slot: %v", err)
	}
	if got.err != nil {
		t.Fatalf("slot wait exhaustion should return a BLOCKED result: %v", got.err)
	}
	result := got.result
	if result == nil {
		t.Fatal("expected mutation result")
	}
	if result.Outcome != OutcomeBLOCKED || result.Killed || !result.Restored {
		t.Fatalf("slot wait exhaustion must block and restore: %+v", result)
	}
	if result.Mutant.Duration != 0 {
		t.Fatalf("wait exhaustion must not start the command: duration=%v output=%q", result.Mutant.Duration, result.Output)
	}
	if !strings.Contains(result.Output, "slots busy") {
		t.Fatalf("wait exhaustion must report busy slots, got %q", result.Output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("wait exhaustion must not start a command; marker state err=%v", err)
	}
	assertFile(t, filepath.Join(dir, "candidate.txt"), "original\n")
	requireIsolatedSlotEmpty(t)
}
