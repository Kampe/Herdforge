package verifier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/containerlifecycle"
)

// TestFAC231ReceiptRegisteredBeforeStart proves the ordering guarantee
// FAC-231 is about: a durable receipt exists in the lifecycle store
// after Create and BEFORE Start. The bug this ticket fixes is a crash
// in the window between Create and Start orphaning a container with no
// receipt — a test that only checks the end state would pass on the
// broken code. This test hooks into the fake's Start call and checks the
// store at that exact point.
func TestFAC231ReceiptRegisteredBeforeStart(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	fake.startHook = func(containerID string) error {
		receipt, getErr := store.Get(containerID)
		if getErr != nil {
			t.Fatalf("store.Get at Start time: %v", getErr)
		}
		if receipt == nil {
			t.Fatal("no receipt exists at Start time — Register must happen before Start")
		}
		if receipt.State != containerlifecycle.StateRegistered {
			t.Fatalf("receipt state at Start = %s, want %s", receipt.State, containerlifecycle.StateRegistered)
		}
		if receipt.TaskRef != "FAC-198/FAC-151" {
			t.Fatalf("receipt TaskRef = %q, want FAC-198/FAC-151", receipt.TaskRef)
		}
		if receipt.Generation == "" {
			t.Fatal("receipt Generation is empty — must be the run nonce for post-crash forensics")
		}
		if receipt.ImageDigest == "" {
			t.Fatal("receipt ImageDigest is empty — must record the resolved image pin digest")
		}
		return nil
	}

	_, _ = runner.Run(context.Background())
}

// TestFAC231RegisterFailureFailsClosed proves that when Register fails
// (here: a pre-existing receipt for the same container ID under a
// different identity causes ErrIdentityConflict), the run fails closed:
// the container is removed and Start is never called. A container that
// exists without a receipt is the exact bug; the runner must not proceed
// past Register.
func TestFAC231RegisterFailureFailsClosed(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)

	preStore, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := preStore.Register(containerlifecycle.Receipt{
		ContainerID: fac198PrimaryContainerID,
		TaskRef:     "conflicting-task",
		Generation:  "conflicting-generation",
	}); err != nil {
		t.Fatalf("pre-register conflicting receipt: %v", err)
	}
	preStore.Close()

	result, runErr := runner.Run(context.Background())
	if runErr == nil {
		t.Fatal("run must fail when Register fails")
	}
	if !errors.Is(runErr, containerlifecycle.ErrIdentityConflict) {
		t.Fatalf("run error = %v, want ErrIdentityConflict in chain", runErr)
	}
	if !fake.removeAttempted || fake.removeID != fac198PrimaryContainerID {
		t.Fatalf("container must be removed on Register failure: attempted=%t id=%q", fake.removeAttempted, fake.removeID)
	}
	for _, call := range fake.callOrder {
		if call == "start" {
			t.Fatal("Start must not be called when Register fails")
		}
	}
	if result.Removed {
		t.Fatal("result.Removed must be false when Register fails before the teardown defer is installed")
	}
}

// TestFAC231MarkStartedTransitionsReceipt proves the receipt transitions
// to "started" after docker Start succeeds, so a crash after Start (but
// before the run concludes) leaves a receipt that reconciliation can
// distinguish from one that never started. The check runs at the
// mountinfo probe — after Start+MarkStarted but before the deferred
// EnsureCleanup transitions the receipt to removed.
func TestFAC231MarkStartedTransitionsReceipt(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	fake.startHook = func(containerID string) error {
		receipt, getErr := store.Get(containerID)
		if getErr != nil {
			t.Fatalf("store.Get at Start: %v", getErr)
		}
		if receipt == nil || receipt.State != containerlifecycle.StateRegistered {
			t.Fatalf("receipt state at Start = %+v, want registered", receipt)
		}
		return nil
	}
	fake.mountInfoHook = func() error {
		receipt, getErr := store.Get(fac198PrimaryContainerID)
		if getErr != nil {
			t.Fatalf("store.Get at mountinfo probe: %v", getErr)
		}
		if receipt == nil {
			t.Fatal("no receipt at mountinfo probe")
		}
		if receipt.State != containerlifecycle.StateStarted {
			t.Fatalf("receipt state after Start = %s, want %s (MarkStarted must fire after Start succeeds)", receipt.State, containerlifecycle.StateStarted)
		}
		return nil
	}

	_, _ = runner.Run(context.Background())
}

// TestFAC231TeardownProvesAbsenceIndependently proves the deferred
// teardown routed through EnsureCleanup still uses the independent
// absence check as the sole authority — not docker rm's own exit
// status. When Remove succeeds and Inspect confirms the container is
// gone, the receipt must be in StateRemoved with AbsenceProved=true,
// and result.Removed must reflect that.
func TestFAC231TeardownProvesAbsenceIndependently(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)

	result, runErr := runner.Run(context.Background())
	if runErr == nil {
		t.Fatal("expected fixed fake verifier failure")
	}

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	receipt, err := store.Get(fac198PrimaryContainerID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if receipt == nil {
		t.Fatal("no receipt after teardown")
	}
	if receipt.State != containerlifecycle.StateRemoved {
		t.Fatalf("receipt state = %s, want %s", receipt.State, containerlifecycle.StateRemoved)
	}
	if !receipt.AbsenceProved {
		t.Fatal("AbsenceProved = false, want true (independent absence check must confirm removal)")
	}
	if !result.Removed {
		t.Fatal("result.Removed = false, want true (must reflect independently proven absence)")
	}
	if fake.postInspect != 1 {
		t.Fatalf("postInspect = %d, want 1 (absence check calls Inspect once after remove)", fake.postInspect)
	}
}

// TestFAC231TeardownSurfacesRemoveError proves teardown errors still
// join into the returned error when routed through EnsureCleanup. When
// docker rm fails and the container is confirmed still present, the
// receipt must be quarantined (not removed), result.Removed must be
// false, and the remove error must be in the error chain.
func TestFAC231TeardownSurfacesRemoveError(t *testing.T) {
	removeErr := errors.New("docker daemon unreachable")
	fake := &fac198DockerFake{removeErr: removeErr}
	runner := newFAC198FakeRunner(t, fake)

	result, runErr := runner.Run(context.Background())
	if runErr == nil {
		t.Fatal("expected run failure")
	}
	if !errors.Is(runErr, removeErr) {
		t.Fatalf("run error = %v, want removeErr in chain", runErr)
	}
	if result.Removed {
		t.Fatal("result.Removed = true, want false (remove failed, container still present)")
	}

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	receipt, err := store.Get(fac198PrimaryContainerID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if receipt == nil {
		t.Fatal("no receipt after teardown")
	}
	if receipt.State != containerlifecycle.StateQuarantined {
		t.Fatalf("receipt state = %s, want %s (failed cleanup must quarantine)", receipt.State, containerlifecycle.StateQuarantined)
	}
}

// TestFAC231ReceiptFieldsAreMeaningful proves the Register field choices
// are populated with meaningful values for post-crash forensics, not
// empty placeholders.
func TestFAC231ReceiptFieldsAreMeaningful(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)
	runner.candidateSHA = strings.Repeat("a", 40)

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	fake.startHook = func(containerID string) error {
		receipt, getErr := store.Get(containerID)
		if getErr != nil {
			t.Fatalf("store.Get: %v", getErr)
		}
		if receipt == nil {
			t.Fatal("no receipt")
		}
		if receipt.ContainerID != fac198PrimaryContainerID {
			t.Fatalf("ContainerID = %q, want %q", receipt.ContainerID, fac198PrimaryContainerID)
		}
		if receipt.TaskRef != "FAC-198/FAC-151" {
			t.Fatalf("TaskRef = %q, want FAC-198/FAC-151 (the verification task the runner serves)", receipt.TaskRef)
		}
		if receipt.Generation == "" {
			t.Fatal("Generation is empty — must be the per-run nonce so reconciliation can distinguish crashed runs")
		}
		if receipt.ImageDigest == "" {
			t.Fatal("ImageDigest is empty — must be the resolved image pin config digest")
		}
		if receipt.CleanupOwner != "hermetic-docker-runner" {
			t.Fatalf("CleanupOwner = %q, want hermetic-docker-runner", receipt.CleanupOwner)
		}
		return nil
	}

	_, _ = runner.Run(context.Background())
}

// TestFAC231ProvenAbsenceOverridesRemoveError proves a remove error does
// NOT fail the run when the container's absence is independently proved.
//
// containerlifecycle.EnsureCleanup is explicit that "an independently
// proved absence is definitive even if remove() itself errored (e.g. it
// lost a race with an out-of-band removal)". If the runner joins the
// remove error on top of EnsureCleanup's verdict, it reintroduces exactly
// the false failure EnsureCleanup exists to suppress: a teardown that
// genuinely succeeded still fails the run because docker rm printed
// something. The receipt must be StateRemoved with AbsenceProved, the run
// must carry no teardown error, and result.Removed must be true.
func TestFAC231ProvenAbsenceOverridesRemoveError(t *testing.T) {
	removeErr := errors.New("Error response from daemon: removal already in progress")
	fake := &fac198DockerFake{removeErr: removeErr, removeErrButGone: true}
	runner := newFAC198FakeRunner(t, fake)

	result, runErr := runner.Run(context.Background())
	if runErr != nil && errors.Is(runErr, removeErr) {
		t.Fatalf("run error contains removeErr (%v), want teardown treated as successful: an independently proved absence is definitive", runErr)
	}
	if !result.Removed {
		t.Fatal("result.Removed = false, want true (absence was independently proved despite the remove error)")
	}

	store, err := containerlifecycle.NewStore(runner.storePath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	receipt, err := store.Get(fac198PrimaryContainerID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if receipt == nil {
		t.Fatal("no receipt after teardown")
	}
	if receipt.State != containerlifecycle.StateRemoved {
		t.Fatalf("receipt state = %s, want %s (proved absence is terminal, not quarantine)", receipt.State, containerlifecycle.StateRemoved)
	}
	if !receipt.AbsenceProved {
		t.Fatal("AbsenceProved = false, want true")
	}
}
