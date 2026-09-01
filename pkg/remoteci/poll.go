package remoteci

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrTerminalFailure = errors.New("remote-ci: required check failed")
	ErrPollingTimeout  = errors.New("remote-ci: polling timed out")
	ErrRetryExhausted  = errors.New("remote-ci: polling retry limit exhausted")
)

type Observation string

const (
	ObservationRegistered     Observation = "registered"
	ObservationPending        Observation = "pending"
	ObservationNoChecks       Observation = "no_checks"
	ObservationUnavailable    Observation = "unavailable"
	ObservationAmbiguous      Observation = "ambiguous"
	ObservationStale          Observation = "stale"
	ObservationFailed         Observation = "failed"
	ObservationPassed         Observation = "passed"
	ObservationTimeout        Observation = "timeout"
	ObservationRetryExhausted Observation = "retry_exhausted"
	ObservationReadbackFailed Observation = "readback_failed"
)

// SettlementStore is the exact durable transaction surface used by Poller.
// *Store is the production implementation; tests inject failures at readback.
type SettlementStore interface {
	Register(Binding) (Settlement, bool, error)
	PersistTerminal(Settlement) (bool, error)
	Load(Binding) (Settlement, error)
}

type PollResult struct {
	Settlement  Settlement  `json:"settlement"`
	Registered  bool        `json:"registered"`
	Polls       int         `json:"polls"`
	Observation Observation `json:"observation"`
}

// Poller registers before observation, retries only provider-transient states,
// persists only exact terminal results, and reads the canonical row back before
// reporting PASS. Context and MaxPolls are independent hard bounds.
type Poller struct {
	Watcher      Watcher
	Store        SettlementStore
	Router       FailureRouter
	PollInterval time.Duration
	MaxPolls     int
}

func (p Poller) Run(ctx context.Context, binding Binding) (PollResult, error) {
	result := PollResult{Observation: ObservationRegistered}
	if ctx == nil || p.Watcher == nil || p.Store == nil || p.PollInterval <= 0 || p.MaxPolls < 1 {
		return result, fmt.Errorf("%w: poller requires context, watcher, store, positive interval, and positive max polls", ErrInvalid)
	}
	if err := binding.Validate(); err != nil {
		return result, err
	}
	_, registered, err := p.Store.Register(binding)
	if err != nil {
		return result, fmt.Errorf("remote-ci: register watch: %w", err)
	}
	result.Registered = registered
	current, err := p.Store.Load(binding)
	if err != nil {
		result.Observation = ObservationReadbackFailed
		return result, fmt.Errorf("remote-ci: registered watch readback: %w", err)
	}
	if err := Settle(binding, current); err != nil {
		result.Observation = bindingErrorObservation(err)
		return result, fmt.Errorf("remote-ci: registered watch identity: %w", err)
	}
	result.Settlement = current
	if current.State != StatePending {
		return finishTerminal(result)
	}

	for result.Polls < p.MaxPolls {
		if err := ctx.Err(); err != nil {
			return pollContextError(result, err)
		}
		settlement, watchErr := p.Watcher.Watch(ctx, binding)
		result.Polls++
		if watchErr != nil {
			result.Observation = watchErrorObservation(watchErr)
			if errors.Is(watchErr, ErrPending) {
				if err := Settle(binding, settlement); err != nil {
					result.Observation = bindingErrorObservation(err)
					return result, fmt.Errorf("remote-ci: pending provider identity: %w", err)
				}
				if settlement.State != StatePending {
					result.Observation = ObservationAmbiguous
					return result, fmt.Errorf("%w: provider returned pending error with state %q", ErrAmbiguous, settlement.State)
				}
				result.Settlement = settlement
			}
			if !retryableWatchError(watchErr) {
				return result, fmt.Errorf("remote-ci: provider observation: %w", watchErr)
			}
			if result.Polls >= p.MaxPolls {
				result.Observation = ObservationRetryExhausted
				return result, fmt.Errorf("%w after %d polls; last provider state: %v", ErrRetryExhausted, result.Polls, watchErr)
			}
			if err := waitForNextPoll(ctx, p.PollInterval); err != nil {
				return pollContextError(result, err)
			}
			continue
		}
		if err := Settle(binding, settlement); err != nil {
			result.Observation = bindingErrorObservation(err)
			return result, fmt.Errorf("remote-ci: provider settlement identity: %w", err)
		}
		if settlement.State == StatePending {
			result.Settlement = settlement
			result.Observation = ObservationPending
			if result.Polls >= p.MaxPolls {
				result.Observation = ObservationRetryExhausted
				return result, fmt.Errorf("%w after %d polls; last provider state: pending", ErrRetryExhausted, result.Polls)
			}
			if err := waitForNextPoll(ctx, p.PollInterval); err != nil {
				return pollContextError(result, err)
			}
			continue
		}

		persistErr := PersistAndRouteTerminal(ctx, p.Store, settlement, p.Router)
		canonical, readErr := p.Store.Load(binding)
		if readErr != nil {
			result.Observation = ObservationReadbackFailed
			return result, errors.Join(persistErr, fmt.Errorf("remote-ci: terminal canonical readback: %w", readErr))
		}
		if err := Settle(binding, canonical); err != nil {
			result.Observation = bindingErrorObservation(err)
			return result, errors.Join(persistErr, fmt.Errorf("remote-ci: terminal canonical identity: %w", err))
		}
		if canonical.State != settlement.State {
			result.Observation = ObservationReadbackFailed
			return result, errors.Join(persistErr, fmt.Errorf("%w: terminal canonical state %q does not match provider state %q", ErrInvalid, canonical.State, settlement.State))
		}
		result.Settlement = canonical
		if persistErr != nil {
			result.Observation = terminalObservation(canonical.State)
			return result, fmt.Errorf("remote-ci: persist or route terminal settlement: %w", persistErr)
		}
		return finishTerminal(result)
	}
	result.Observation = ObservationRetryExhausted
	return result, fmt.Errorf("%w after %d polls", ErrRetryExhausted, result.Polls)
}

func finishTerminal(result PollResult) (PollResult, error) {
	result.Observation = terminalObservation(result.Settlement.State)
	switch result.Settlement.State {
	case StatePassed:
		return result, nil
	case StateFailed:
		return result, ErrTerminalFailure
	default:
		return result, fmt.Errorf("%w: expected terminal settlement, got %q", ErrInvalid, result.Settlement.State)
	}
}

func terminalObservation(state State) Observation {
	if state == StatePassed {
		return ObservationPassed
	}
	return ObservationFailed
}

func watchErrorObservation(err error) Observation {
	switch {
	case errors.Is(err, ErrPending):
		return ObservationPending
	case errors.Is(err, ErrNoChecks):
		return ObservationNoChecks
	case errors.Is(err, ErrUnavailable):
		return ObservationUnavailable
	case errors.Is(err, ErrAmbiguous):
		return ObservationAmbiguous
	case errors.Is(err, ErrStale):
		return ObservationStale
	default:
		return ObservationUnavailable
	}
}

func bindingErrorObservation(err error) Observation {
	if errors.Is(err, ErrStale) {
		return ObservationStale
	}
	return ObservationAmbiguous
}

func retryableWatchError(err error) bool {
	return errors.Is(err, ErrPending) || errors.Is(err, ErrNoChecks) || errors.Is(err, ErrUnavailable)
}

func waitForNextPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func pollContextError(result PollResult, err error) (PollResult, error) {
	if errors.Is(err, context.DeadlineExceeded) {
		last := result.Observation
		result.Observation = ObservationTimeout
		return result, fmt.Errorf("%w after %d polls; last observation %s: %v", ErrPollingTimeout, result.Polls, last, err)
	}
	return result, err
}
