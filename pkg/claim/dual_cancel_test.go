package claim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

func dualCancelFixture(t *testing.T) (coord, launch *SQLiteLeaseStore, journal string, key LeaseKey, now time.Time) {
	t.Helper()
	dir := t.TempDir()
	var err error
	coord, err = NewSQLiteLeaseStore(filepath.Join(dir, "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coord.Close() })
	launch, err = NewSQLiteLeaseStore(filepath.Join(dir, "launch-claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { launch.Close() })
	key = LeaseKey{Repo: "herdforge", Provider: "kaneo", Project: "proj", TaskRef: "FAC-713"}
	now = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	return coord, launch, filepath.Join(dir, "journal.jsonl"), key, now
}

func dualReq(coord, launch *SQLiteLeaseStore, journal string, key LeaseKey, now time.Time) DualCancelRequest {
	return DualCancelRequest{
		Key:             key,
		Owner:           CoordinatorDispatchOwner,
		Generation:      1,
		Now:             now,
		Coordinator:     coord,
		Launch:          launch,
		CoordinatorPath: "coordinator-store",
		LaunchPath:      "launch-store",
		JournalPath:     journal,
	}
}

func TestCancelMatchingGeneration_FAC685CoordinatorReleasedLaunchActive(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	c1, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.Release(ctx, key, CoordinatorDispatchOwner, c1.Generation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(ctx, key, "worker-pid9-0123456789abcdef", "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	result, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, now))
	if err != nil {
		t.Fatalf("dual cancel: %v", err)
	}
	if result.Coordinator.Disposition != DispositionAlreadyReleased {
		t.Fatalf("coordinator disposition=%s, want already-released", result.Coordinator.Disposition)
	}
	if result.Launch.Disposition != DispositionReleased {
		t.Fatalf("launch disposition=%s, want released", result.Launch.Disposition)
	}
	if result.Coordinator.Store != "coordinator-store" || result.Launch.Store != "launch-store" {
		t.Fatalf("store names = %+v", result)
	}
	if result.Launch.TaskRef != "FAC-713" || result.Launch.Generation != 1 {
		t.Fatalf("launch report = %+v", result.Launch)
	}
	active, err := launch.ActiveClaims(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("launch still active: %+v", active)
	}
	row, err := launch.LeaseByGeneration(ctx, key, "worker-pid9-0123456789abcdef", 1)
	if err != nil || row == nil || row.Status != StatusReleased || row.ReleasedAt == nil {
		t.Fatalf("launch gen 1 readback = %+v err=%v", row, err)
	}
}

func TestCancelMatchingGeneration_FAC668LaunchGen1CoordinatorGen2Unchanged(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	first, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.Release(ctx, key, CoordinatorDispatchOwner, first.Generation, now); err != nil {
		t.Fatal(err)
	}
	second, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != 2 {
		t.Fatalf("coordinator generation = %d, want 2", second.Generation)
	}
	if _, err := launch.Acquire(ctx, key, "launch-pid3-aaaaaaaaaaaaaaaa", "launch", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	result, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, now))
	if err != nil {
		t.Fatalf("dual cancel: %v", err)
	}
	if result.Launch.Disposition != DispositionReleased {
		t.Fatalf("launch disposition=%s", result.Launch.Disposition)
	}
	current, err := coord.CurrentLease(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Generation != 2 || current.Status != StatusActive {
		t.Fatalf("coordinator current = %+v, want active generation 2", current)
	}
	launchActive, err := launch.ActiveClaims(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(launchActive) != 0 {
		t.Fatalf("launch still active: %+v", launchActive)
	}
}

func TestCancelMatchingGeneration_ReleasesBothAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	otherKey := key
	otherKey.TaskRef = "FAC-OTHER"
	other, err := coord.Acquire(ctx, otherKey, CoordinatorDispatchOwner, "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	req := dualReq(coord, launch, journal, key, now)
	first, err := CancelMatchingGeneration(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Coordinator.Disposition != DispositionReleased || first.Launch.Disposition != DispositionReleased {
		t.Fatalf("first result = %+v", first)
	}
	second, err := CancelMatchingGeneration(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Coordinator.Disposition != DispositionAlreadyReleased || second.Launch.Disposition != DispositionAlreadyReleased {
		t.Fatalf("idempotent result = %+v", second)
	}
	still, err := coord.LeaseByGeneration(ctx, otherKey, CoordinatorDispatchOwner, other.Generation)
	if err != nil || still == nil || still.Status != StatusActive {
		t.Fatalf("unrelated row mutated: %+v err=%v", still, err)
	}
}

func TestCancelMatchingGeneration_RefusesLiveWorkerHeldLockWrongOwnerGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	t.Run("live-worker", func(t *testing.T) {
		coord, launch, journal, key, n := dualCancelFixture(t)
		if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := launch.Acquire(ctx, key, "herdr-session-live", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, n)); !errors.Is(err, ErrDualCancelLiveWorker) {
			t.Fatalf("err=%v, want live worker", err)
		}
		assertBothActive(t, coord, launch, key, n)
	})

	t.Run("wrong-owner", func(t *testing.T) {
		coord, launch, journal, key, n := dualCancelFixture(t)
		if _, err := coord.Acquire(ctx, key, "someone-else", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, n)); !errors.Is(err, ErrDualCancelWrongOwner) {
			t.Fatalf("err=%v, want wrong owner", err)
		}
		assertBothActive(t, coord, launch, key, n)
	})

	t.Run("wrong-generation", func(t *testing.T) {
		coord, launch, journal, key, n := dualCancelFixture(t)
		if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		req := dualReq(coord, launch, journal, key, n)
		req.Generation = 9
		if _, err := CancelMatchingGeneration(ctx, req); !errors.Is(err, ErrDualCancelWrongGeneration) {
			t.Fatalf("err=%v, want wrong generation", err)
		}
		assertBothActive(t, coord, launch, key, n)
	})

	t.Run("provider-lock", func(t *testing.T) {
		coord, launch, journal, key, n := dualCancelFixture(t)
		c1, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", n, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := coord.AcquireProviderLock(ctx, key, CoordinatorDispatchOwner, c1.Generation, "transition-owner", time.Minute, n); err != nil {
			t.Fatal(err)
		}
		if _, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, n)); !errors.Is(err, ErrDualCancelProviderLock) {
			t.Fatalf("err=%v, want provider lock", err)
		}
		assertBothActive(t, coord, launch, key, n)
	})

	t.Run("held-scope", func(t *testing.T) {
		coord, launch, journal, key, n := dualCancelFixture(t)
		holds, err := lifecycle.NewHoldAuthorityWithClock(filepath.Join(t.TempDir(), "holds.db"), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { holds.Close() })
		c1, err := coord.AcquireWithIdentity(ctx, key, CoordinatorDispatchOwner, "worker", "", key.Repo, "worker", "smith", n, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", n, time.Minute); err != nil {
			t.Fatal(err)
		}
		id := lifecycle.HoldIdentity{Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}
		if _, err := holds.Hold(ctx, id, "operator", "pause", "held", 1, nil); err != nil {
			t.Fatal(err)
		}
		req := dualReq(coord, launch, journal, key, n)
		req.HoldReader = holds
		if _, err := CancelMatchingGeneration(ctx, req); !errors.Is(err, ErrDualCancelHeld) {
			t.Fatalf("err=%v, want held", err)
		}
		got, err := coord.LeaseByGeneration(ctx, key, CoordinatorDispatchOwner, c1.Generation)
		if err != nil || got == nil || got.Status != StatusActive {
			t.Fatalf("held cancel mutated coordinator: %+v err=%v", got, err)
		}
		assertBothActive(t, coord, launch, key, n)
	})

	t.Run("partial-store", func(t *testing.T) {
		coord, _, journal, key, n := dualCancelFixture(t)
		req := dualReq(coord, nil, journal, key, n)
		req.Launch = nil
		if _, err := CancelMatchingGeneration(ctx, req); !errors.Is(err, ErrDualCancelPartialStore) {
			t.Fatalf("err=%v, want partial store", err)
		}
	})

	_ = now
}

func TestCancelMatchingGeneration_SecondStoreFailureLeavesRecoverableJournal(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	req := dualReq(coord, launch, journal, key, now)
	req.AfterFirstRelease = func() error { return errors.New("simulated launch-ok coordinator-fail") }
	_, err := CancelMatchingGeneration(ctx, req)
	if !errors.Is(err, ErrDualCancelRecoverable) {
		t.Fatalf("err=%v, want recoverable journal", err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("journal missing after partial failure: %v", err)
	}
	launchRow, err := launch.CurrentLease(ctx, key)
	if err != nil || launchRow == nil || launchRow.Status != StatusReleased {
		t.Fatalf("launch after partial = %+v err=%v", launchRow, err)
	}
	coordRow, err := coord.CurrentLease(ctx, key)
	if err != nil || coordRow == nil || coordRow.Status != StatusActive {
		t.Fatalf("coordinator after partial = %+v err=%v (want still active; journal recovers)", coordRow, err)
	}

	req.AfterFirstRelease = nil
	result, err := CancelMatchingGeneration(ctx, req)
	if err != nil {
		t.Fatalf("journal retry: %v", err)
	}
	if result.Coordinator.Disposition != DispositionReleased && result.Coordinator.Disposition != DispositionAlreadyReleased {
		t.Fatalf("retry coordinator disposition=%s", result.Coordinator.Disposition)
	}
	coordRow, err = coord.CurrentLease(ctx, key)
	if err != nil || coordRow == nil || coordRow.Status != StatusReleased {
		t.Fatalf("coordinator after retry = %+v err=%v", coordRow, err)
	}
}

func TestCancelMatchingGeneration_ExpiredWorkerIsStaleNotLive(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(ctx, key, "herdr-session-dead", "worker", "", now.Add(-2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, now)); err != nil {
		t.Fatalf("expired worker should be releasable: %v", err)
	}
	active, err := launch.ActiveClaims(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("expired worker launch still active: %+v", active)
	}
}

func TestCancelMatchingGeneration_CLISafeReportHasNoOwnerSecrets(t *testing.T) {
	ctx := context.Background()
	coord, launch, journal, key, now := dualCancelFixture(t)
	if _, err := coord.Acquire(ctx, key, CoordinatorDispatchOwner, "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(ctx, key, "worker-pid1-0123456789abcdef", "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	result, err := CancelMatchingGeneration(ctx, dualReq(coord, launch, journal, key, now))
	if err != nil {
		t.Fatal(err)
	}
	blob := result.Coordinator.Store + result.Coordinator.TaskRef + result.Launch.Store + result.Launch.TaskRef + string(result.Coordinator.Disposition) + string(result.Launch.Disposition)
	if strings.Contains(blob, "pid") || strings.Contains(blob, CoordinatorDispatchOwner) {
		t.Fatalf("report leaked owner identity: %+v", result)
	}
}

func assertBothActive(t *testing.T, coord, launch *SQLiteLeaseStore, key LeaseKey, now time.Time) {
	t.Helper()
	c, err := coord.ActiveClaims(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	l, err := launch.ActiveClaims(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 1 || len(l) != 1 {
		t.Fatalf("want both stores still active, coordinator=%+v launch=%+v", c, l)
	}
}
