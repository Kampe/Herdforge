package claim

import (
	"context"
	"sync"
	"testing"
)

func TestClaimManager_ConcurrentClaims(t *testing.T) {
	cm := NewClaimManager()
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

	if !cm.IsClaimed("FAC-15") {
		t.Errorf("expected task FAC-15 to be claimed")
	}

	cm.ReleaseClaim("FAC-15")
	if cm.IsClaimed("FAC-15") {
		t.Errorf("expected task FAC-15 claim to be released")
	}
}
