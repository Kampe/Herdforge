package dispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestDispatchHealth_Race(t *testing.T) {
	board := &timeoutBoard{hangList: true}
	cfg := &config.Config{TaskProvider: config.TaskProvider{
		Type: "kaneo", ProjectID: "p",
		Deadlines: config.OpDeadlines{List: "25ms"},
	}}
	d := NewDispatcher(cfg, board, nil)
	d.Compensator = noopComp{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				_, _ = d.listTasksBound(context.Background(), "p", "")
				_ = d.ProviderStatus()
				_ = d.ProviderHealth()
			}
		}()
	}
	wg.Wait()
	if d.ProviderHealth().State != ProviderBlocked {
		t.Fatalf("state=%s", d.ProviderStatus())
	}
	if provider.ClassifyOpError(&provider.TimeoutError{Cause: context.DeadlineExceeded}) != provider.OpTimeout {
		t.Fatal("ClassifyOpError consumer sanity")
	}
}
