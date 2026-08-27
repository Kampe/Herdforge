package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FAC-607: a provider-bound read that dies at the operator's `timeout` boundary
// produces rc=124 and ZERO stdout. Reported repeatedly against `herd deps
// check`, `board-sync`, `board-audit` and `herd lost` while Kaneo canonical
// reads succeeded and live Herdr panes were healthy.
//
// Zero output is indistinguishable from a clean result. The operator cannot
// tell "the provider never answered" from "there was nothing to report", so the
// safe reading and the dangerous reading look identical -- and the dangerous one
// is the one that gets acted on, because a silent command looks like a passing
// command.
//
// The bound itself is not the defect. The silence is. A read that cannot
// complete must say so, name what it was doing, and be classified UNKNOWN --
// never clean, never empty.

// ReadOutcome classifies how a bounded provider read ended. Timeout and Empty
// are deliberately separate: collapsing them is the entire defect.
type ReadOutcome string

const (
	// ReadComplete is a read that finished within its budget.
	ReadComplete ReadOutcome = "complete"
	// ReadTimedOut is a read that ran out of budget. It is UNKNOWN, not empty.
	ReadTimedOut ReadOutcome = "timed-out"
	// ReadFailed is a read the provider actively refused or errored.
	ReadFailed ReadOutcome = "failed"
)

// ReadDiagnostics is what a bounded read reports when it cannot finish.
//
// Every field exists because an operator asked for it during the incident:
// which provider, how far it got, what bound was applied, and what the last
// known-good state was so they can judge how stale a fallback would be.
type ReadDiagnostics struct {
	Provider     string        `json:"provider"`
	Phase        string        `json:"phase"`
	Budget       time.Duration `json:"budget"`
	Elapsed      time.Duration `json:"elapsed"`
	Outcome      ReadOutcome   `json:"outcome"`
	LastRevision string        `json:"last_successful_cache_revision,omitempty"`
	Err          string        `json:"error,omitempty"`
}

// Unknown reports whether this read failed to establish the truth. A consumer
// must never treat an Unknown read as a clean or empty result.
func (d ReadDiagnostics) Unknown() bool {
	return d.Outcome == ReadTimedOut || d.Outcome == ReadFailed
}

// String is the one line an operator sees instead of silence.
func (d ReadDiagnostics) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "provider read %s: provider=%s phase=%q budget=%s elapsed=%s",
		d.Outcome, d.Provider, d.Phase, d.Budget, d.Elapsed.Round(time.Millisecond))
	if d.LastRevision != "" {
		fmt.Fprintf(&b, " last-successful-cache-revision=%s", d.LastRevision)
	} else {
		b.WriteString(" last-successful-cache-revision=none")
	}
	if d.Err != "" {
		fmt.Fprintf(&b, " error=%q", d.Err)
	}
	if d.Unknown() {
		b.WriteString("; this is UNKNOWN, not an empty or clean result -- do not infer clean state from it")
	}
	return b.String()
}

// Phases records how far a read got, so a timeout can name the phase it died
// in rather than reporting only that it died.
type Phases struct {
	mu      sync.Mutex
	current string
}

// Enter marks the phase now in progress.
func (p *Phases) Enter(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = name
}

// Current returns the phase last entered, or "unstarted".
func (p *Phases) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(p.current) == "" {
		return "unstarted"
	}
	return p.current
}

// BoundedRead runs fn under its own deadline and, whatever happens, returns
// diagnostics that are never empty.
//
// The internal budget must be SHORTER than any external `timeout` wrapper, so
// this reports before the operator's bound kills the process. A read that is
// killed from outside cannot explain itself; that is exactly the rc=124 silence
// this exists to prevent.
//
// fn receives the phase recorder and is expected to call Enter as it advances.
// A read that records no phases still reports "unstarted", which is itself
// evidence: it says the provider was never reached.
func BoundedRead[T any](ctx context.Context, providerName string, budget time.Duration, lastRevision string,
	fn func(context.Context, *Phases) (T, error)) (T, ReadDiagnostics, error) {

	var zero T
	phases := &Phases{}
	diag := ReadDiagnostics{
		Provider:     providerName,
		Budget:       budget,
		LastRevision: lastRevision,
	}

	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	started := time.Now()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn(readCtx, phases)
		done <- result{v: v, err: err}
	}()

	select {
	case r := <-done:
		diag.Elapsed = time.Since(started)
		diag.Phase = phases.Current()
		if r.err != nil {
			diag.Outcome = ReadFailed
			diag.Err = r.err.Error()
			return zero, diag, fmt.Errorf("%s", diag.String())
		}
		diag.Outcome = ReadComplete
		return r.v, diag, nil

	case <-readCtx.Done():
		// Report from the parent goroutine rather than waiting on fn: fn may be
		// blocked in a call that ignores context, and waiting for it here would
		// reproduce the silence this function exists to prevent.
		diag.Elapsed = time.Since(started)
		diag.Phase = phases.Current()
		diag.Outcome = ReadTimedOut
		diag.Err = readCtx.Err().Error()
		return zero, diag, fmt.Errorf("%s", diag.String())
	}
}
