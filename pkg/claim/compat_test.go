package claim

import (
	"context"
	"sync"
	"testing"
)

// TestClaimManager_ClaimTask_ConcurrentClaims is the pre-FAC-120
// TestClaimManager_ConcurrentClaims test, ported verbatim in spirit onto
// the migration-path constructor and legacy ClaimTask/ReleaseClaim/
// IsClaimedLegacy methods, proving old call sites keep working (module a
// one-line constructor swap) after the FAC-120 rewrite.
func TestClaimManager_ClaimTask_ConcurrentClaims(t *testing.T) {
	cm := NewInMemoryClaimManager()
	ctx := context.Background()

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			_, err := cm.ClaimTask(ctx, nil, "FAC-15", string(rune('A'+workerID)), "/tmp/wt")
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful claim out of 10 concurrent requests, got %d", successCount)
	}

	if !cm.IsClaimedLegacy("FAC-15") {
		t.Errorf("expected task FAC-15 to be claimed")
	}

	cm.ReleaseClaim("FAC-15")
	if cm.IsClaimedLegacy("FAC-15") {
		t.Errorf("expected task FAC-15 claim to be released")
	}
}

func TestClaimManager_ClaimTask_ReturnsRecordAndRejectsSecondClaim(t *testing.T) {
	cm := NewInMemoryClaimManager()
	ctx := context.Background()

	rec, err := cm.ClaimTask(ctx, nil, "FAC-16", "worker-a", "/tmp/wt-a")
	if err != nil {
		t.Fatalf("first ClaimTask: %v", err)
	}
	if rec.TaskRef != "FAC-16" || rec.WorkerID != "worker-a" || rec.WorktreePath != "/tmp/wt-a" {
		t.Fatalf("unexpected ClaimRecord: %+v", rec)
	}

	if _, err := cm.ClaimTask(ctx, nil, "FAC-16", "worker-b", "/tmp/wt-b"); err == nil {
		t.Fatal("expected a second ClaimTask for the same taskRef to fail")
	}

	cm.ReleaseClaim("FAC-16")
	if _, err := cm.ClaimTask(ctx, nil, "FAC-16", "worker-b", "/tmp/wt-b"); err != nil {
		t.Fatalf("expected ClaimTask to succeed again after ReleaseClaim, got %v", err)
	}
}

func TestClaimManager_ReleaseClaim_NoopWhenNotClaimed(t *testing.T) {
	cm := NewInMemoryClaimManager()
	// Old ReleaseClaim returned nothing and never panicked/errored on a
	// taskRef nobody holds; preserve that.
	cm.ReleaseClaim("FAC-NEVER-CLAIMED")
	if cm.IsClaimedLegacy("FAC-NEVER-CLAIMED") {
		t.Fatal("expected an unclaimed taskRef to stay unclaimed")
	}
}

// TestInMemoryLeaseStore_NotCrossProcessSafeIsExpected documents (via the
// same exactly-one-winner shape used elsewhere) that InMemoryLeaseStore's
// guarantee is process-local only, matching the pre-FAC-120 ClaimManager
// it replaces; SQLiteLeaseStore is what adds cross-process safety.
func TestInMemoryLeaseStore_ConcurrentAcquire_ExactlyOneWinnerInProcess(t *testing.T) {
	mgr := NewClaimManager(NewInMemoryLeaseStore())
	ctx := context.Background()
	key := testKey("FAC-17")

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: string(rune('A' + i)), Role: "herd-smith", TaskRole: "herd-smith"})
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful claim out of 20 concurrent requests, got %d", successCount)
	}
}
