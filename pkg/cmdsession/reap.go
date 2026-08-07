package cmdsession

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// Disposition is what one sweep decided about one command session.
type Disposition string

const (
	// DispositionReaped: descriptors closed, waited exactly once.
	DispositionReaped Disposition = "reaped"
	// DispositionSettledAbsent: the exact identity was proven gone.
	DispositionSettledAbsent Disposition = "settled_absent"
	// DispositionPreservedLive: still running, or owned by another
	// authority. Left completely untouched — not closed, not signalled.
	DispositionPreservedLive Disposition = "preserved_live"
	// DispositionDeferredDetached: a detached descendant is still
	// unaccounted for, so the session stays owned and observable.
	DispositionDeferredDetached Disposition = "deferred_detached"
	// DispositionBlocked: identity or completion was ambiguous. Durable
	// BLOCKED evidence was written; nothing was touched.
	DispositionBlocked Disposition = "blocked"
)

// Observation is what a Probe can prove about an exact PID right now.
// It carries no judgement: presence, parentage, and the start token are
// facts; whether they are safe to act on is decided here.
type Observation struct {
	Present    bool
	ParentPID  int
	StartToken string
}

// Probe re-reads a live process's exact identity. Returning an error must
// mean "could not determine", never "absent" — an unreadable probe fails
// closed rather than authorizing a teardown.
type Probe func(Identity) (Observation, error)

// Closer closes every descriptor bound to a session — stdin, stdout, stderr,
// and the PTY controller. It returns the kinds that are still open; a
// non-empty result blocks the reap, because a session with a live PTY writer
// is not finished no matter what the parent reported.
type Closer func(Identity) (stillOpen []string, err error)

// Waiter waits the exact session and returns its exit code. It must be
// callable at most once per session; reap guarantees that by refusing to
// re-enter a session that already reached a terminal state.
type Waiter func(Identity) (exitCode int, err error)

// Reap performs the exact teardown for one completed command session:
// re-prove identity, close every descriptor, then wait exactly once.
//
// It refuses, with durable BLOCKED evidence and without touching anything,
// when identity cannot be re-proved (probe failure, start-token mismatch from
// PID reuse, unexpected parentage), when the session is still producing output
// after its terminal receipt, or when a descriptor stayed open. It refuses
// outright to act on a session that has not recorded a terminal outcome:
// live work is never torn down by this path.
func Reap(store *Store, key Key, probe Probe, closer Closer, waiter Waiter) (Disposition, error) {
	r, err := store.Get(key)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", fmt.Errorf("%w: key=%s", ErrUnknownSession, key)
	}
	switch r.State {
	case StateReaped:
		return DispositionReaped, nil
	case StateSettledAbsent:
		return DispositionSettledAbsent, nil
	case StateBlocked:
		return DispositionBlocked, nil
	case StateRunning:
		return "", fmt.Errorf("%w: key=%s", ErrNotCompleted, key)
	}

	// pkg/toolchild (FAC-188) is the teardown authority for lane-owned tool
	// children. Tracking one here is for visibility only.
	if r.LaneOwned {
		return DispositionPreservedLive, nil
	}
	if r.DetachedOpen > 0 {
		return DispositionDeferredDetached, nil
	}
	if r.LastOutputAt != nil && r.CompletedAt != nil && r.LastOutputAt.After(*r.CompletedAt) {
		return blocked(store, key, fmt.Sprintf(
			"output at %s postdates the terminal receipt at %s; completion is ambiguous",
			r.LastOutputAt.UTC().Format("2006-01-02T15:04:05Z"), r.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")))
	}

	obs, err := probe(r.Identity)
	if err != nil {
		return blocked(store, key, fmt.Sprintf("identity probe failed for pid %d: %v", r.PID, err))
	}
	if !obs.Present {
		// Nothing is left to close or wait. Settle the receipt so it stops
		// counting as a retained terminal, but do not claim a wait that this
		// process did not perform.
		if err := store.markSettledAbsent(key, fmt.Sprintf("pid %d absent at reconcile; not waited by this process", r.PID)); err != nil {
			return "", err
		}
		return DispositionSettledAbsent, nil
	}
	if obs.StartToken != r.StartToken {
		return blocked(store, key, fmt.Sprintf(
			"start-token mismatch on pid %d (%q now, %q at spawn): pid was reused", r.PID, obs.StartToken, r.StartToken))
	}
	if obs.ParentPID != r.ParentPID {
		return blocked(store, key, fmt.Sprintf(
			"unknown parentage for pid %d (parent %d now, %d at spawn)", r.PID, obs.ParentPID, r.ParentPID))
	}

	stillOpen, err := closer(r.Identity)
	if err != nil {
		return blocked(store, key, fmt.Sprintf("closing descriptors for pid %d failed: %v", r.PID, err))
	}
	if len(stillOpen) > 0 {
		sort.Strings(stillOpen)
		return blocked(store, key, fmt.Sprintf("descriptors still open for pid %d: %v", r.PID, stillOpen))
	}

	code, err := waiter(r.Identity)
	if err != nil {
		return blocked(store, key, fmt.Sprintf("waiting pid %d failed: %v", r.PID, err))
	}
	if err := store.markReaped(key, &code); err != nil {
		return "", err
	}
	return DispositionReaped, nil
}

func blocked(store *Store, key Key, reason string) (Disposition, error) {
	if err := store.MarkBlocked(key, reason); err != nil {
		return "", err
	}
	return DispositionBlocked, nil
}

// Report is one reconciliation sweep's outcome. Every slice holds session
// keys and is sorted before Reconcile returns.
type Report struct {
	Reaped    []string `json:"reaped"`
	Settled   []string `json:"settled_absent"`
	Preserved []string `json:"preserved_live"`
	Deferred  []string `json:"deferred_detached"`
	Blocked   []string `json:"blocked"`
}

// Reconcile sweeps every retained receipt and disposes of each one.
//
// Completed sessions are reaped. Running sessions whose exact identity is
// still present and provable are preserved untouched — an active bounded CLI
// child is never interrupted. A running session whose PID is gone is a lost
// readback (the coordinator crashed or lost the pipe): it is recorded as such
// and settled. Anything ambiguous becomes durable BLOCKED evidence.
//
// It only ever acts on sessions this store already has a receipt for. No
// process is ever matched by name, command, group, or age.
func Reconcile(store *Store, probe Probe, closer Closer, waiter Waiter) (Report, error) {
	receipts, err := store.ListRetained()
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, r := range receipts {
		d, err := disposeOne(store, r, probe, closer, waiter)
		if err != nil {
			return Report{}, err
		}
		rep.add(d, r.Key.String())
	}
	sort.Strings(rep.Reaped)
	sort.Strings(rep.Settled)
	sort.Strings(rep.Preserved)
	sort.Strings(rep.Deferred)
	sort.Strings(rep.Blocked)
	return rep, nil
}

func disposeOne(store *Store, r Receipt, probe Probe, closer Closer, waiter Waiter) (Disposition, error) {
	if r.State == StateCompleted {
		return Reap(store, r.Key, probe, closer, waiter)
	}
	// Running: never torn down on this path. Only classified.
	if r.LaneOwned {
		return DispositionPreservedLive, nil
	}
	obs, err := probe(r.Identity)
	if err != nil {
		return blocked(store, r.Key, fmt.Sprintf("identity probe failed for pid %d: %v", r.PID, err))
	}
	if obs.Present && obs.StartToken != r.StartToken {
		return blocked(store, r.Key, fmt.Sprintf(
			"start-token mismatch on pid %d (%q now, %q at spawn): pid was reused", r.PID, obs.StartToken, r.StartToken))
	}
	if obs.Present && obs.ParentPID != r.ParentPID {
		return blocked(store, r.Key, fmt.Sprintf(
			"unknown parentage for pid %d (parent %d now, %d at spawn)", r.PID, obs.ParentPID, r.ParentPID))
	}
	if obs.Present {
		return DispositionPreservedLive, nil
	}
	// Gone with no terminal receipt: the tool call's outcome was never read
	// back. Record that honestly, then dispose of it like any completion.
	if err := store.MarkCompleted(r.Key, OutcomeLostReadback, nil); err != nil {
		return "", err
	}
	return Reap(store, r.Key, probe, closer, waiter)
}

func (rep *Report) add(d Disposition, key string) {
	switch d {
	case DispositionReaped:
		rep.Reaped = append(rep.Reaped, key)
	case DispositionSettledAbsent:
		rep.Settled = append(rep.Settled, key)
	case DispositionPreservedLive:
		rep.Preserved = append(rep.Preserved, key)
	case DispositionDeferredDetached:
		rep.Deferred = append(rep.Deferred, key)
	case DispositionBlocked:
		rep.Blocked = append(rep.Blocked, key)
	}
}

// SystemProbe is the production probe. It reuses pkg/toolchild's exact
// process adapter (FAC-188) rather than reimplementing process lookup, so
// both packages read identity — PID, parent, and start token — the same way.
func SystemProbe(id Identity) (Observation, error) {
	node, ok, err := toolchild.SystemTree{}.Lookup(id.PID)
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{}, nil
	}
	return Observation{Present: true, ParentPID: node.ParentPID, StartToken: node.Identity.StartToken}, nil
}

// ErrForeignSession is what the cross-process seams return: a sweep running
// outside the coordinator holds no descriptors and is not the session's
// parent, so it can neither close nor wait. Reap turns that into BLOCKED
// evidence, which is the honest answer for a retained background terminal
// only its owning process can actually tear down.
var ErrForeignSession = errors.New("cmdsession: session is not owned by this process")

// ForeignCloser is the Closer for an out-of-process sweep (e.g. the CLI).
func ForeignCloser(Identity) ([]string, error) { return nil, ErrForeignSession }

// ForeignWaiter is the Waiter for an out-of-process sweep (e.g. the CLI).
func ForeignWaiter(Identity) (int, error) { return 0, ErrForeignSession }

// ExecSeams builds the in-process Closer and Waiter for a command session the
// caller spawned, from the descriptors it holds. descriptors maps a kind
// ("stdin", "stdout", "stderr", "pty") to the endpoint; every one is closed
// before the wait, and any that fails to close is reported still-open so the
// reap fails closed instead of leaving a writer attached.
//
// The returned Waiter calls cmd.Wait at most once no matter how many sweeps
// reach it, because exec.Cmd.Wait is not safe to call twice.
func ExecSeams(cmd *exec.Cmd, descriptors map[string]io.Closer) (Closer, Waiter) {
	closer := func(Identity) ([]string, error) {
		var stillOpen []string
		kinds := make([]string, 0, len(descriptors))
		for kind := range descriptors {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			c := descriptors[kind]
			if c == nil {
				continue
			}
			if err := c.Close(); err != nil && !isAlreadyClosed(err) {
				stillOpen = append(stillOpen, kind)
			}
		}
		return stillOpen, nil
	}
	var once sync.Once
	var code int
	var waitErr error
	waiter := func(Identity) (int, error) {
		once.Do(func() {
			err := cmd.Wait()
			if err == nil {
				code = cmd.ProcessState.ExitCode()
				return
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
				return
			}
			waitErr = err
		})
		return code, waitErr
	}
	return closer, waiter
}

// isAlreadyClosed treats a double close as success: the descriptor is gone,
// which is the state the reap is trying to reach.
func isAlreadyClosed(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) ||
		strings.Contains(err.Error(), "file already closed")
}
