package containerlifecycle

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Remover force-removes the exact container ID.
type Remover func(ctx context.Context, containerID string) error

// AbsenceChecker reports whether containerID no longer exists.
type AbsenceChecker func(ctx context.Context, containerID string) (bool, error)

// EnsureCleanup is the single compensation path every container launch
// should defer, regardless of how the run ended — success, failed test,
// timeout, or cancellation. Callers must pass a teardown context that
// outlives the run's own (possibly already-cancelled or expired)
// context, so cleanup still runs on a timeout/cancellation path; it never
// acts on a container the store has no receipt for, and it never marks a
// container removed without an independently confirmed absence.
//
// It records expectedTerminalState via MarkAwaitingCleanup itself before
// attempting removal (a no-op if the receipt is already awaiting cleanup
// with a state recorded by an earlier, more specific call), so a caller
// never has to remember two separate calls to get outcome classification
// right.
//
// The independently confirmed absence check — not docker's own exit
// status for the remove command — is the sole authority for whether a
// container is actually gone. remove() is attempted, but even if it
// errors (including a "no such container" error, whose exact wording
// varies by docker version and isn't safe to pattern-match as success),
// absent() is still checked: if it confirms the container is gone, that
// settles it. This avoids trusting removal-side error text as the signal
// for an outcome only an independent read can actually prove.
func EnsureCleanup(ctx context.Context, store *Store, containerID, expectedTerminalState string, remove Remover, absent AbsenceChecker) error {
	receipt, err := store.Get(containerID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownContainer, containerID)
	}
	if receipt.State == StateRemoved || receipt.State == StateQuarantined {
		return nil // already terminal; idempotent no-op
	}
	if receipt.State != StateAwaitingCleanup {
		if err := store.MarkAwaitingCleanup(containerID, expectedTerminalState); err != nil {
			return fmt.Errorf("containerlifecycle: record terminal outcome for %s: %w", containerID, err)
		}
	}

	removeErr := remove(ctx, containerID)

	ok, absentErr := absent(ctx, containerID)
	if absentErr != nil {
		reason := fmt.Sprintf("absence check failed: %v", absentErr)
		if removeErr != nil {
			reason = fmt.Sprintf("absence check failed: %v (remove also failed: %v)", absentErr, removeErr)
		}
		_ = store.MarkQuarantined(containerID, reason)
		return fmt.Errorf("containerlifecycle: confirm absence of %s: %w", containerID, absentErr)
	}
	if !ok {
		reason := "remove reported success but container is still present"
		if removeErr != nil {
			reason = fmt.Sprintf("remove failed: %v", removeErr)
		}
		_ = store.MarkQuarantined(containerID, reason)
		return fmt.Errorf("containerlifecycle: %s still present (remove error: %v)", containerID, removeErr)
	}
	// Confirmed gone, regardless of what remove() reported — an
	// independently proved absence is definitive even if remove() itself
	// errored (e.g. it lost a race with an out-of-band removal).
	return store.MarkRemoved(containerID, true)
}

// GenerationLive reports whether taskRef's lease/session generation is
// still the active one. A receipt whose generation is no longer live was
// orphaned by a crashed, timed-out, or superseded run and is safe to
// reclaim.
type GenerationLive func(taskRef, generation string) bool

// ReconcileReport is one sweep's outcome. Slices are sorted by container
// ID before Reconcile returns, for deterministic comparison in tests and
// logs regardless of receipt insertion order.
type ReconcileReport struct {
	// DryRun is false for a real Reconcile call (this type's zero value
	// matches that). `herd containers reconcile`'s dry-run mode sets it
	// true and reuses this exact type/schema for its preview output —
	// Reclaimed/Quarantined/Skipped mean "would be" rather than "were"
	// in that case — so the CLI's --json output has one consistent
	// shape whether or not --apply was passed.
	DryRun      bool     `json:"dry_run"`
	Reclaimed   []string `json:"reclaimed"`   // cleaned up this sweep, absence proved
	Quarantined []string `json:"quarantined"` // cleanup attempted and failed; needs operator review
	Skipped     []string `json:"skipped"`     // owning generation still live; left untouched
}

// Reconcile sweeps every non-terminal receipt and reclaims the ones
// whose owning generation is no longer live. It only ever acts on
// containers this store already has a receipt for — an unrelated or
// preexisting container is never a candidate no matter what its name,
// image, or `docker ps` status says (see AuditUnowned for those).
// Reconcile is idempotent: a receipt already terminal (removed or
// quarantined) is excluded by ListNonTerminal, so running Reconcile
// again after a sweep reclaims nothing new for containers it already
// resolved — an agent interrupted between create and start is recovered
// exactly once.
func Reconcile(ctx context.Context, store *Store, live GenerationLive, remove Remover, absent AbsenceChecker) (ReconcileReport, error) {
	receipts, err := store.ListNonTerminal()
	if err != nil {
		return ReconcileReport{}, err
	}
	var report ReconcileReport
	for _, r := range receipts {
		if live(r.TaskRef, r.Generation) {
			report.Skipped = append(report.Skipped, r.ContainerID)
			continue
		}
		// "orphaned" is a synthetic terminal state: Reconcile only ever
		// runs against receipts whose owning generation is no longer
		// live, so by construction nothing recorded a real outcome for
		// them. EnsureCleanup won't overwrite an expected_terminal_state
		// a runner already set via its own MarkAwaitingCleanup call
		// before the crash/timeout that orphaned it.
		if err := EnsureCleanup(ctx, store, r.ContainerID, "orphaned", remove, absent); err != nil {
			report.Quarantined = append(report.Quarantined, r.ContainerID)
			continue
		}
		report.Reclaimed = append(report.Reclaimed, r.ContainerID)
	}
	sort.Strings(report.Reclaimed)
	sort.Strings(report.Quarantined)
	sort.Strings(report.Skipped)
	return report, nil
}

// StaleGenerationLive returns a GenerationLive that treats a receipt's
// generation as still live only if some receipt sharing that exact
// task_ref/generation was updated within staleAfter of now(). This is a
// deliberately conservative stand-in for real lease/session authority:
// FAC-198's hermetic runner (the actual owner of generation liveness)
// isn't merged yet, so there is no coordinator-side authority to query.
// Once it lands, callers should supply a GenerationLive backed by that
// authority instead — this helper stays useful as its fallback/offline
// mode, and is what `herd containers reconcile` uses today. On a store
// read error it fails closed by reporting live=true, never reclaiming
// anything it couldn't positively confirm as dead.
//
// Only registered/started/awaiting_cleanup receipts count as evidence of
// a live generation. removed and quarantined are deliberately excluded:
// those states are only ever written by EnsureCleanup itself — including
// when Reconcile is the one calling it for a sibling receipt in the same
// generation a moment earlier in this same sweep. If a terminal state's
// updated_at counted as "recent activity", reclaiming one dead
// generation's container would refresh a timestamp that then makes every
// OTHER container in that same dead generation look live, so a sweep
// would reclaim exactly one per generation per pass instead of all of
// them — the opposite of what Reconcile promises.
func StaleGenerationLive(store *Store, staleAfter time.Duration, now func() time.Time) GenerationLive {
	return func(taskRef, generation string) bool {
		receipts, err := store.ListAll()
		if err != nil {
			return true
		}
		for _, r := range receipts {
			if r.TaskRef != taskRef || r.Generation != generation {
				continue
			}
			if r.State == StateRemoved || r.State == StateQuarantined {
				continue
			}
			if now().Sub(r.UpdatedAt) <= staleAfter {
				return true
			}
		}
		return false
	}
}
