package resources

// FAC-829: production wiring for the observer.
//
// Everything policy-shaped lives in runObserverLoop, which knows nothing about
// clocks, locks or files. This file supplies the real ones and nothing else, so
// the cadence rules stay testable without a host.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// observerLockRetry is how often acquisition retries inside LockWait. It must
// be positive and no greater than the wait, which FileLockProvider enforces.
const observerLockRetry = 200 * time.Millisecond

// RunObserver takes the canonical observer lock and samples until the context
// is cancelled or the configured lifetime elapses.
//
// It is a FOREGROUND service: no daemonising, no background goroutine, no
// detached process. The caller's context is the only lifetime, so the operator
// who started it can stop it.
//
// A competing observer is refused promptly with ErrObserverBusy rather than
// queued. Two observers publishing to one status path would interleave whole-
// file writes, and the survivor would be whichever wrote last -- which is not
// exclusivity, it is a race with a tidy filename.
func RunObserver(ctx context.Context, cfg ObserverConfig, lockProvider LockProvider) (ObserverStatus, error) {
	cfg, err := cfg.Validate()
	if err != nil {
		return ObserverStatus{}, fmt.Errorf("observer configuration refused: %w", err)
	}
	if lockProvider == nil {
		lockProvider = FileLockProvider{}
	}

	lock, err := lockProvider.Acquire(ctx, cfg.LockPath, cfg.LockWait, observerLockRetry)
	if err != nil {
		if ctx.Err() != nil {
			return ObserverStatus{}, ctx.Err()
		}
		// The lock is held, unsupported, or unusable. Every one of those is a
		// refusal to start, never a reason to run unlocked: an unlocked
		// observer would publish over whichever one already owns the file.
		return ObserverStatus{}, fmt.Errorf("%w (%s, %s): %v",
			ErrObserverBusy, cfg.LockPath, ObserverScopeExplanation(ObserverLockScope()), err)
	}
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
	}()

	scope := ObserverLockScope()
	id := ObserverIdentity{
		PID:           os.Getpid(),
		StartedAt:     stampUTC(time.Now()),
		LockPath:      cfg.LockPath,
		LockScope:     string(scope),
		IntervalMS:    millis(cfg.Interval),
		LifetimeMS:    millis(cfg.Lifetime),
		SampleTimeout: millis(cfg.SampleTimeout),
	}

	deps := observerDeps{
		now:  func() time.Time { return time.Now().UTC() },
		wait: waitFor,
		sample: func(sampleCtx context.Context, at time.Time) (AdmissionReport, error) {
			// Admit performs the native OS probes. Its own clock reads happen
			// per probe; Report(at) then revalidates the decision against this
			// tick, so an observation that aged out renders as a refusal
			// rather than a stale admit.
			report := Admit(sampleCtx).Report(at)
			if sampleCtx.Err() != nil {
				// The decision above is already fail-closed on missing probes;
				// the context error is recorded so the cause is visible rather
				// than inferred from an UNKNOWN reading.
				return report, fmt.Errorf("sample deadline: %w", sampleCtx.Err())
			}
			return report, nil
		},
		publish: func(status ObserverStatus) error {
			return writeObserverStatus(cfg.StatusPath, status)
		},
	}

	status, err := runObserverLoop(ctx, cfg, id, deps)
	if err != nil && errors.Is(err, context.Canceled) {
		// Operator-initiated shutdown is a normal exit, and the terminal
		// status has already been published by the loop.
		return status, nil
	}
	return status, err
}

// waitFor blocks for d, or until ctx is done, whichever comes first.
//
// A non-positive duration means the tick is already due: it yields to
// cancellation and returns immediately rather than sleeping, so a slow sample
// cannot turn into a catch-up burst.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ObserverStatusSummary is the one-line form for an operator, so reading the
// status does not require reading JSON.
func ObserverStatusSummary(status ObserverStatus, at time.Time) string {
	usable, why := ObserverUsable(status, at)
	state := "USABLE"
	if !usable {
		state = "NOT USABLE (" + why + ")"
	}
	return fmt.Sprintf("observer pid=%d scope=%s ticks=%d skipped=%d published=%s expires=%s decision=%s %s",
		status.Observer.PID, status.Observer.LockScope, status.TotalTicks, status.SkippedTicks,
		status.PublishedAt, status.ExpiresAt, status.Latest.Report.Decision, state)
}

// compile-time proof that the shipped provider satisfies the interface the
// observer asks for, on every platform (the non-unix twin refuses at runtime).
var _ LockProvider = FileLockProvider{}
