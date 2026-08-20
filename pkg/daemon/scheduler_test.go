package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPulseSchedulerRunsImmediatelyAndOnCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	err := RunPulseScheduler(ctx, PulseSchedulerOptions{
		Interval: time.Millisecond,
		MaxTicks: 3,
	}, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("RunPulseScheduler() error = %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("callback calls = %d, want 3", got)
	}
}

func TestRunPulseSchedulerContinuesAfterSweepError(t *testing.T) {
	var calls atomic.Int32
	err := RunPulseScheduler(context.Background(), PulseSchedulerOptions{Interval: time.Millisecond, MaxTicks: 2}, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("transient sweep failure")
		}
		return nil
	})
	if err == nil || err.Error() != "pulse scheduler: tick 1: transient sweep failure" {
		t.Fatalf("error = %v, want first sweep error", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("callback calls = %d, want 2 despite first error", got)
	}
}

func TestRunPulseSchedulerStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	err := RunPulseScheduler(ctx, PulseSchedulerOptions{}, func(context.Context) error {
		calls.Add(1)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback calls = %d, want one immediate sweep", got)
	}
}
