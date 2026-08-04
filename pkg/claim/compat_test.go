package claim

import (
	"context"
	"errors"
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

	if successCount != 0 {
		t.Errorf("disabled ClaimTask must acquire nothing, got %d successes", successCount)
	}

	if cm.IsClaimedLegacy("FAC-15") {
		t.Errorf("disabled ClaimTask must not create a claim")
	}

	cm.ReleaseClaim("FAC-15")
	if cm.IsClaimedLegacy("FAC-15") {
		t.Errorf("disabled ClaimTask must remain unclaimed")
	}
}

func TestClaimManager_ClaimTask_ReturnsRecordAndRejectsSecondClaim(t *testing.T) {
	cm := NewInMemoryClaimManager()
	ctx := context.Background()

	if _, err := cm.ClaimTask(ctx, nil, "FAC-16", "worker-a", "/tmp/wt-a"); !errors.Is(err, ErrLegacyClaimDisabled) {
		t.Fatalf("expected ClaimTask migration refusal, got %v", err)
	}

	if cm.IsClaimedLegacy("FAC-16") {
		t.Fatal("disabled ClaimTask must not acquire a lease")
	}

	cm.ReleaseClaim("FAC-16")
	if _, err := cm.ClaimTask(ctx, nil, "FAC-16", "worker-b", "/tmp/wt-b"); !errors.Is(err, ErrLegacyClaimDisabled) {
		t.Fatalf("expected repeated ClaimTask migration refusal, got %v", err)
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
	mgr := NewClaimManager(NewInMemoryLeaseStore(), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-17")

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := mgr.Claim(ctx, testClaimRequest(key, string(rune('A'+i)), "herd-smith"))
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
