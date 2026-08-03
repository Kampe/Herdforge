package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// Race suite: concurrent pulse + health reads must not corrupt state or claim
// under BLOCKED (go test -race).
func TestProviderHealth_Race(t *testing.T) {
	tp := &timeoutProvider{
		failAfter: 0,
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityHigh},
		},
	}
	e := newTimeoutEngine(tp)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = e.RunPulse(context.Background(), "worker")
				_ = e.ProviderStatus()
				_ = e.ProviderHealth()
			}
		}()
	}
	wg.Wait()
	if tp.claims.Load() != 0 {
		t.Fatalf("race claimed under timeout: %d", tp.claims.Load())
	}
	if e.ProviderHealth().State != ProviderBlocked {
		t.Fatalf("state=%s", e.ProviderStatus())
	}
}

func TestForgeLoop_NoDispatchWhileBlocked_Mutation(t *testing.T) {
	// If isBlocked check is removed from ActionDispatch, this fails when
	// failAfter allows a to-do after recovery without clearing block incorrectly.
	tp := &timeoutProvider{failAfter: 0, tasks: []*provider.Task{
		{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	}}
	cfg := &config.Config{TaskProvider: config.TaskProvider{
		Type: "kaneo", ProjectID: "p1",
		Deadlines: config.OpDeadlines{List: "40ms", Mutate: "40ms"},
	}}
	e := NewEngine(cfg, tp, nil, nil, nil, nil)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{0, 2}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}
	_ = e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 2})
	if len(d.actions) != 0 {
		t.Fatalf("dispatch under block: %v logs=%v", d.actions, logs)
	}
}
