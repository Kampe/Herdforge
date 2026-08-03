package provider

import (
	"context"
	"errors"
	"math"
	"time"
)

// Default read-retry policy. Mutations must never use RetryRead — ambiguous
// writes go through ReconcileStatus instead of blind re-apply.
const (
	DefaultReadMaxAttempts = 3
	DefaultReadBaseBackoff = 50 * time.Millisecond
	DefaultReadMaxBackoff  = 400 * time.Millisecond
)

// Clock abstracts time for deterministic retry tests.
type Clock interface {
	Now() time.Time
	// After returns a channel that delivers once d has elapsed. Tests may
	// use a fake that closes immediately or on demand.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RetryPolicy configures capped exponential backoff for idempotent reads.
// Jitter is optional; when JitterFraction is 0, backoff is deterministic.
type RetryPolicy struct {
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	JitterFraction float64 // 0..1; 0 = no jitter (deterministic)
	Clock          Clock
	// RetryIf decides whether an error is retryable. nil → defaultRetryable.
	RetryIf func(error) bool
}

// DefaultReadRetry is the safe default for GetTask / ListTasks only.
func DefaultReadRetry() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    DefaultReadMaxAttempts,
		BaseBackoff:    DefaultReadBaseBackoff,
		MaxBackoff:     DefaultReadMaxBackoff,
		JitterFraction: 0, // deterministic by default (tests + fleet status)
		Clock:          realClock{},
	}
}

func (p RetryPolicy) normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultReadMaxAttempts
	}
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = DefaultReadBaseBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultReadMaxBackoff
	}
	if p.Clock == nil {
		p.Clock = realClock{}
	}
	if p.RetryIf == nil {
		p.RetryIf = defaultRetryable
	}
	return p
}

// defaultRetryable retries timeouts and ProviderError with Retryable=true.
// Non-retryable provider errors (4xx except 429) and readback drift do not retry.
func defaultRetryable(err error) bool {
	if err == nil {
		return false
	}
	if IsTimeout(err) {
		return true
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// backoffDelay returns the delay before attempt index i (0-based after first try).
// attempt 0 → BaseBackoff, then exponential, capped at MaxBackoff.
// When JitterFraction is 0 the sequence is fully deterministic.
func (p RetryPolicy) backoffDelay(attempt int) time.Duration {
	p = p.normalize()
	if attempt < 0 {
		attempt = 0
	}
	// 2^attempt * base
	mult := math.Pow(2, float64(attempt))
	d := time.Duration(float64(p.BaseBackoff) * mult)
	if d > p.MaxBackoff {
		d = p.MaxBackoff
	}
	if p.JitterFraction <= 0 {
		return d
	}
	// Deterministic jitter from attempt index (no math/rand) so tests stay
	// stable while still spreading retries under load when enabled.
	frac := p.JitterFraction
	if frac > 1 {
		frac = 1
	}
	// Offset in [-frac, +frac] of d using attempt parity.
	delta := float64(d) * frac * (0.5 - float64(attempt%3)/2.0)
	out := time.Duration(float64(d) + delta)
	if out < 0 {
		return 0
	}
	return out
}

// RetryRead invokes fn until it succeeds, RetryIf rejects the error, attempts
// are exhausted, or ctx is done. fn receives the same ctx (caller should
// derive per-attempt deadlines inside fn if needed).
//
// NEVER use for ClaimTask / UpdateStatus / AddComment — those are not
// idempotent under ambiguous timeout; use ReconcileStatus instead.
func RetryRead(ctx context.Context, policy RetryPolicy, fn func(context.Context) error) error {
	policy = policy.normalize()
	if ctx == nil {
		ctx = context.Background()
	}
	var last error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return AsTimeout("", "RetryRead", OpGet, 0, err)
		}
		last = fn(ctx)
		if last == nil {
			return nil
		}
		if !policy.RetryIf(last) {
			return last
		}
		// No sleep after the final attempt.
		if attempt+1 >= policy.MaxAttempts {
			break
		}
		delay := policy.backoffDelay(attempt)
		select {
		case <-ctx.Done():
			return AsTimeout("", "RetryRead", OpGet, 0, ctx.Err())
		case <-policy.Clock.After(delay):
		}
	}
	return last
}
