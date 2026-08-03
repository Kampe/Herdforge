package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock delivers After immediately (zero delay) for deterministic tests,
// while still counting how many waits occurred.
type fakeClock struct {
	waits atomic.Int32
}

func (f *fakeClock) Now() time.Time { return time.Unix(0, 0) }
func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.waits.Add(1)
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, int64(d))
	return ch
}

func TestBackoffDelay_DeterministicExponentialCap(t *testing.T) {
	p := RetryPolicy{
		BaseBackoff: 50 * time.Millisecond,
		MaxBackoff:  400 * time.Millisecond,
	}
	want := []time.Duration{
		50 * time.Millisecond,  // 2^0
		100 * time.Millisecond, // 2^1
		200 * time.Millisecond, // 2^2
		400 * time.Millisecond, // 2^3 capped
		400 * time.Millisecond, // still capped
	}
	for i, w := range want {
		got := p.backoffDelay(i)
		if got != w {
			t.Errorf("attempt %d: got %v want %v", i, got, w)
		}
	}
	// Re-run: deterministic.
	if p.backoffDelay(2) != 200*time.Millisecond {
		t.Fatal("backoff must be deterministic with zero jitter")
	}
}

func TestRetryRead_SucceedsAfterTransientTimeouts(t *testing.T) {
	var n atomic.Int32
	clk := &fakeClock{}
	policy := RetryPolicy{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		Clock:       clk,
	}
	err := RetryRead(context.Background(), policy, func(ctx context.Context) error {
		c := n.Add(1)
		if c < 3 {
			return &TimeoutError{Cause: context.DeadlineExceeded, Kind: OpGet}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryRead: %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts=%d want 3", n.Load())
	}
	if clk.waits.Load() != 2 {
		t.Fatalf("waits=%d want 2 (between 3 attempts)", clk.waits.Load())
	}
}

func TestRetryRead_DoesNotRetryNonRetryable(t *testing.T) {
	var n atomic.Int32
	err := RetryRead(context.Background(), DefaultReadRetry(), func(ctx context.Context) error {
		n.Add(1)
		return &ProviderError{StatusCode: 404, Message: "not found", Retryable: false}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != 1 {
		t.Fatalf("attempts=%d want 1 (non-retryable)", n.Load())
	}
}

func TestRetryRead_ExhaustsAttempts(t *testing.T) {
	var n atomic.Int32
	clk := &fakeClock{}
	policy := RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Clock: clk}
	err := RetryRead(context.Background(), policy, func(ctx context.Context) error {
		n.Add(1)
		return &TimeoutError{Cause: context.DeadlineExceeded}
	})
	if !IsTimeout(err) {
		t.Fatalf("want timeout after exhaust, got %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts=%d want 3", n.Load())
	}
}

func TestRetryRead_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var n atomic.Int32
	err := RetryRead(ctx, DefaultReadRetry(), func(context.Context) error {
		n.Add(1)
		return nil
	})
	if !IsTimeout(err) {
		t.Fatalf("want cancel error, got %v", err)
	}
	if n.Load() != 0 {
		t.Fatalf("fn must not run on already-canceled ctx, ran %d", n.Load())
	}
}

func TestRetryRead_RetriesRetryableProviderError(t *testing.T) {
	var n atomic.Int32
	clk := &fakeClock{}
	policy := RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Clock: clk}
	err := RetryRead(context.Background(), policy, func(context.Context) error {
		if n.Add(1) == 1 {
			return &ProviderError{StatusCode: 503, Message: "unavailable", Retryable: true}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts=%d", n.Load())
	}
}

// Non-vacuity: a policy that always retries forever would hang; MaxAttempts
// is the hard cap. Mutating MaxAttempts to 0 normalizes to default (still finite).
func TestRetryRead_ZeroMaxAttemptsUsesDefault(t *testing.T) {
	var n atomic.Int32
	clk := &fakeClock{}
	err := RetryRead(context.Background(), RetryPolicy{MaxAttempts: 0, Clock: clk, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(context.Context) error {
		n.Add(1)
		return &TimeoutError{Cause: context.DeadlineExceeded}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != DefaultReadMaxAttempts {
		t.Fatalf("attempts=%d want default %d", n.Load(), DefaultReadMaxAttempts)
	}
}

func TestDefaultRetryable_PlainError(t *testing.T) {
	if defaultRetryable(errors.New("nope")) {
		t.Fatal("plain error must not retry")
	}
}
