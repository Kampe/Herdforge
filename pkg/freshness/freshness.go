// Package freshness distinguishes "the source said nothing is there" from
// "the source did not answer".
//
// FAC-662: external domains -- the board provider, the container runtime, disk
// -- were consumed as if a failure were an answer. A Kaneo ListTasks timeout
// produced an empty task list, and an empty task list is indistinguishable from
// a drained queue, so a selector reported "0 claimable" and lanes went idle
// against a board that was full. Measured repeatedly on the live fleet:
// `herd attention` returned BLOCKED(provider_timeout) after 30s, `herd deps
// check` reproduced ListProjectRelations deadline exceeded after 2m, and the
// terminal board matrix reported UNKNOWN with no way to tell which it was.
//
// This is the same defect this codebase has produced in eight other places: an
// absence read as a definitive negative. The difference here is that the absence
// comes from outside, so it cannot be fixed by being more careful at the call
// site -- every consumer would have to remember, and one of them never will.
//
// So state is explicit and a caller cannot accidentally treat UNKNOWN as EMPTY:
// the value and its posture travel together, and reading the value requires
// acknowledging the posture.
package freshness

import (
	"fmt"
	"strings"
	"time"
)

// State is what the adapter actually knows.
type State string

const (
	// StateFresh means the source answered, just now.
	StateFresh State = "FRESH"
	// StateStale means the source did not answer, but a previous answer is
	// still held. Usable with its age stated, never silently.
	StateStale State = "STALE"
	// StateUnknown means the source did not answer and nothing is held. NOT
	// empty: nothing whatsoever is known.
	StateUnknown State = "UNKNOWN"
)

// Reading carries a value together with how much it can be trusted.
type Reading[T any] struct {
	value      T
	State      State     `json:"state"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	// Err is why the live read failed, when it did. Present for STALE and
	// UNKNOWN so a consumer can report the cause rather than a bare posture.
	Err string `json:"error,omitempty"`
	// Recovery is the single narrow action that would restore the source. It is
	// advice, never something the adapter performs: recovering an external
	// domain is the operator's call.
	Recovery string `json:"recovery,omitempty"`
}

// Value returns the held value and whether it may be used at all.
//
// UNKNOWN returns ok=false, so a consumer that ignores the second return gets
// the zero value and not a plausible-looking empty result. That is the whole
// point: an empty task list must not be reachable by forgetting to check.
func (r Reading[T]) Value() (T, bool) {
	if r.State == StateUnknown {
		var zero T
		return zero, false
	}
	return r.value, true
}

// MustExplain returns a human sentence a consumer can report verbatim, so an
// unknown posture is never rendered as a count.
func (r Reading[T]) MustExplain(now time.Time) string {
	switch r.State {
	case StateFresh:
		return fmt.Sprintf("%s: fresh", orUnnamed(r.Source))
	case StateStale:
		return fmt.Sprintf("%s: STALE by %s (%s); showing the last known answer, not a current one%s",
			orUnnamed(r.Source), now.Sub(r.ObservedAt).Round(time.Second), orNoError(r.Err), recoverySuffix(r.Recovery))
	default:
		return fmt.Sprintf("%s: UNKNOWN (%s); nothing is known, which is NOT the same as nothing being there%s",
			orUnnamed(r.Source), orNoError(r.Err), recoverySuffix(r.Recovery))
	}
}

// Fresh records a successful live read.
func Fresh[T any](source string, at time.Time, v T) Reading[T] {
	return Reading[T]{value: v, State: StateFresh, ObservedAt: at, Source: source}
}

// Degrade turns a failed live read into STALE when a previous answer is held,
// or UNKNOWN when none is. It never invents a value.
func Degrade[T any](prev Reading[T], source string, err error, recovery string) Reading[T] {
	msg := "no answer"
	if err != nil {
		msg = err.Error()
	}
	if prev.State == StateFresh || prev.State == StateStale {
		return Reading[T]{
			value: prev.value, State: StateStale, ObservedAt: prev.ObservedAt,
			Source: source, Err: msg, Recovery: recovery,
		}
	}
	return Reading[T]{State: StateUnknown, Source: source, Err: msg, Recovery: recovery}
}

// StaleBeyond reports whether a held answer is too old to act on. A caller that
// needs certainty can refuse rather than act on history.
func (r Reading[T]) StaleBeyond(now time.Time, limit time.Duration) bool {
	if r.State != StateStale || limit <= 0 {
		return false
	}
	return now.Sub(r.ObservedAt) > limit
}

func orUnnamed(s string) string {
	if strings.TrimSpace(s) == "" {
		return "source"
	}
	return s
}

func orNoError(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no error reported"
	}
	return s
}

func recoverySuffix(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ". Recovery: " + s
}
