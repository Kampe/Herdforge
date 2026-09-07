package provider

import (
	"context"
	"testing"
)

func TestCoreCapabilityCancellationDoesNotPoisonLaterRead(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kaneoRunCLI = func(ctx context.Context, _ string, _ ...string) (*CLIResult, error) {
		calls++
		if calls == 1 {
			cancel()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &CLIResult{Stdout: []byte("Usage: kaneo-core task get\n --core")}, nil
	}
	k := &KaneoProvider{UseCLI: true, CoreTaskReads: true, ProjectID: "p1"}
	if err := k.requireCoreRead(ctx); err == nil {
		t.Fatal("canceled probe accepted")
	}
	if err := k.requireCoreRead(context.Background()); err != nil {
		t.Fatalf("healthy later probe poisoned: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want a new probe after cancellation, calls=%d", calls)
	}
}

func TestCoreCapabilityWaiterCanCancel(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	entered := make(chan struct{})
	release := make(chan struct{})
	kaneoRunCLI = func(context.Context, string, ...string) (*CLIResult, error) {
		close(entered)
		<-release
		return &CLIResult{Stdout: []byte("Usage: kaneo-core task get\n --core")}, nil
	}
	k := &KaneoProvider{CoreTaskReads: true, ProjectID: "p1"}
	first := make(chan error, 1)
	go func() { first <- k.requireCoreRead(context.Background()) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := k.requireCoreRead(ctx)
	close(release)
	if err == nil {
		t.Error("canceled waiter accepted")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := k.requireCoreRead(context.Background()); err != nil {
		t.Fatal(err)
	}
}
