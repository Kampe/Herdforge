package main

// FAC-829: `herd resources --watch`, an opt-in observer mode on the existing
// command.
//
// The one-shot interface is untouched: without --watch this file is not
// reached, and `herd resources`, `--json`, `--gate` and `--selftest` behave
// exactly as before.
//
// This is a FOREGROUND service, not a daemon. It does not fork, detach, or
// install anything. The operator's terminal owns it and Ctrl-C stops it, which
// is the property that makes it safe to offer under a resource hold.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// observerStatusJSON renders a published status for --json consumers.
func observerStatusJSON(status resources.ObserverStatus) (string, error) {
	body, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode observer status: %w", err)
	}
	return string(body), nil
}

// Exit codes for observer mode. 3 is already "refused" for `--gate`, so the
// observer reuses it for a refusal to START and keeps 1 for real failures.
const (
	observerExitRefused = 3
)

// observerFlags are the operator-settable bounds. Paths are deliberately absent:
// the lock and status locations are canonical (see resources.ObserverLockPath),
// because a caller-supplied lock path lets two observers each believe they are
// the singleton, and a caller-supplied status path could be aimed at the
// operational guard's report or at source.
type observerFlags struct {
	interval      time.Duration
	lifetime      time.Duration
	sampleTimeout time.Duration
	// provided records which duration flags the operator actually set. An
	// ABSENT flag takes the default; a flag SET to zero or a negative value is
	// an explicit mistake and is refused. Collapsing those two cases is how a
	// deliberate `--interval 0` gets silently turned into 30s.
	provided map[string]bool
}

func (f observerFlags) was(name string) bool { return f.provided[name] }

// runResourcesObserver runs the bounded observer until the lifetime elapses or
// a signal arrives. It prints nothing per tick: the status file is the output,
// and a chatty sampler would be its own log-volume problem.
func runResourcesObserver(f observerFlags) int {
	cfg := resources.DefaultObserverConfig()
	if f.was("interval") {
		cfg.Interval = f.interval
		// Keep the default relationship between interval and per-sample
		// timeout when the operator moved only the interval.
		if !f.was("sample-timeout") {
			cfg.SampleTimeout = f.interval / 2
		}
	}
	if f.was("lifetime") {
		cfg.Lifetime = f.lifetime
	}
	if f.was("sample-timeout") {
		cfg.SampleTimeout = f.sampleTimeout
	}

	validated, err := cfg.Validate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resources --watch: %v\n", err)
		return observerExitRefused
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scope := resources.ObserverLockScope()
	fmt.Fprintf(os.Stderr, "resources observer: interval=%s lifetime=%s sample-timeout=%s\n",
		validated.Interval, validated.Lifetime, validated.SampleTimeout)
	fmt.Fprintf(os.Stderr, "resources observer: status=%s\n", validated.StatusPath)
	fmt.Fprintf(os.Stderr, "resources observer: lock=%s scope=%s (%s)\n",
		validated.LockPath, scope, resources.ObserverScopeExplanation(scope))
	fmt.Fprintln(os.Stderr, "resources observer: "+resources.ObserverAuthorityNote)

	status, err := resources.RunObserver(ctx, validated, nil)
	if err != nil {
		if errors.Is(err, resources.ErrObserverBusy) {
			fmt.Fprintf(os.Stderr, "resources --watch: %v\n", err)
			return observerExitRefused
		}
		fmt.Fprintf(os.Stderr, "resources --watch: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "resources observer: stopped after %d tick(s), %d skipped\n",
		status.TotalTicks, status.SkippedTicks)
	return 0
}

// runResourcesObserverStatus reads the published status and reports whether it
// is current enough to be worth anything.
//
// This is the consumer half of the contract, and it exists so the expiry check
// is demonstrated by shipped code rather than only described in a comment: an
// old file or an exited observer authorizes nothing.
func runResourcesObserverStatus(asJSON bool) int {
	path := resources.ObserverStatusPath()
	status, err := resources.ReadObserverStatus(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resources --observer-status: %v\n", err)
		return observerExitRefused
	}
	now := time.Now().UTC()
	if asJSON {
		// The status is already the wire shape; print it verbatim so a
		// consumer sees exactly what was published, including the expiry.
		body, merr := observerStatusJSON(status)
		if merr != nil {
			fmt.Fprintf(os.Stderr, "resources --observer-status: %v\n", merr)
			return 1
		}
		fmt.Println(body)
	} else {
		fmt.Println(resources.ObserverStatusSummary(status, now))
	}
	if usable, _ := resources.ObserverUsable(status, now); !usable {
		// Not an error: reporting an expired or refusing observation IS the
		// job. The non-zero code lets a caller gate on it without parsing.
		return observerExitRefused
	}
	return 0
}
