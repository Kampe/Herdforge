package reviewledger

import (
	"fmt"
	"strings"
)

const CoordinatorAbortAuthority = "coordinator"

// AbortOpts identifies one exact review launch to abort without writing a
// reviewer verdict. Partial evidence (packet, inbox, logs) is left in place.
type AbortOpts struct {
	SHA       string
	Reviewer  string
	Lease     string
	SessionID string
	Pane      string
	Task      string
	Reason    string
	Artifact  string
}

func validateCoordinatorAbort(rows []LedgerRow, opts AbortOpts) error {
	if strings.TrimSpace(opts.SHA) == "" || strings.TrimSpace(opts.Reviewer) == "" ||
		strings.TrimSpace(opts.Lease) == "" || strings.TrimSpace(opts.SessionID) == "" ||
		strings.TrimSpace(opts.Reason) == "" {
		return fmt.Errorf("coordinator abort requires sha, reviewer, lease, session, and reason")
	}
	if _, err := canonicalLaunchForAbort(rows, opts); err != nil {
		return err
	}
	for _, row := range rows {
		if row.Event != string(EventVerdict) || row.SHA != opts.SHA || row.Reviewer != opts.Reviewer {
			continue
		}
		if row.Verdict == string(VerdictPASS) || row.Verdict == string(VerdictFAIL) || row.Verdict == string(VerdictBLOCKED) {
			return fmt.Errorf("coordinator abort refuses a candidate with existing %s from reviewer %s", row.Verdict, opts.Reviewer)
		}
	}
	return nil
}

// canonicalLaunchForAbort returns the last EventRecord that binds this abort
// to an exact candidate, reviewer, lease, and (when recorded) session/pane/task.
func canonicalLaunchForAbort(rows []LedgerRow, opts AbortOpts) (LedgerRow, error) {
	var sameReviewerSHA, leaseMatches []LedgerRow
	for _, row := range rows {
		if row.Event != string(EventRecord) || row.SHA != opts.SHA || row.Reviewer != opts.Reviewer {
			continue
		}
		sameReviewerSHA = append(sameReviewerSHA, row)
		if row.Lease == opts.Lease {
			leaseMatches = append(leaseMatches, row)
		}
	}
	if len(sameReviewerSHA) == 0 {
		return LedgerRow{}, fmt.Errorf("coordinator abort requires a canonical launch record for reviewer %s", opts.Reviewer)
	}
	if len(leaseMatches) == 0 {
		return LedgerRow{}, fmt.Errorf("coordinator abort lease does not bind the canonical launch")
	}
	launch := leaseMatches[len(leaseMatches)-1]
	if sid := strings.TrimSpace(launch.SessionID); sid != "" && sid != opts.SessionID {
		return LedgerRow{}, fmt.Errorf("coordinator abort session does not bind the canonical launch")
	}
	// Roster/manifest always carry pane_id. Production launch-provenance rows
	// often omit pane. Only a recorded launch pane is identity; an empty
	// launch pane is not a mismatch against a live roster pane.
	if recorded := strings.TrimSpace(launch.Pane); recorded != "" && recorded != strings.TrimSpace(opts.Pane) {
		return LedgerRow{}, fmt.Errorf("coordinator abort pane does not bind the canonical launch")
	}
	if task := strings.TrimSpace(opts.Task); task != "" && strings.TrimSpace(launch.Task) != task {
		return LedgerRow{}, fmt.Errorf("coordinator abort task does not bind the canonical launch")
	}
	return launch, nil
}

// CheckCoordinatorAbort reports whether an abort would be admitted without writing.
func (l *Ledger) CheckCoordinatorAbort(opts AbortOpts) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	return validateCoordinatorAbort(rows, opts)
}

// CoordinatorAbort appends a coordinator-abort event. It never writes EventVerdict
// and never creates a harvest queue entry. Mismatched or incomplete identity is
// refused. An existing terminal verdict from the same reviewer for the same SHA
// is refused so abort cannot override review authority.
func (l *Ledger) CoordinatorAbort(opts AbortOpts) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	if err := validateCoordinatorAbort(rows, opts); err != nil {
		return err
	}
	if _, ok := MatchingCoordinatorAbort(rows, opts.SHA, opts.Reviewer, opts.Lease, opts.SessionID); ok {
		return nil
	}
	return l.appendRow(l.Path, &LedgerRow{
		Event:        string(EventCoordinatorAbort),
		SHA:          opts.SHA,
		CandidateSHA: opts.SHA,
		Reviewer:     opts.Reviewer,
		Lease:        opts.Lease,
		SessionID:    opts.SessionID,
		Pane:         opts.Pane,
		Task:         opts.Task,
		Reason:       opts.Reason,
		Artifact:     opts.Artifact,
		Authority:    CoordinatorAbortAuthority,
		Status:       "aborted",
	})
}

// MatchingCoordinatorAbort returns the latest abort bound to this exact
// reviewer, candidate, lease, and session.
func MatchingCoordinatorAbort(rows []LedgerRow, sha, reviewer, lease, session string) (LedgerRow, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Event != string(EventCoordinatorAbort) {
			continue
		}
		if r.SHA == sha && r.Reviewer == reviewer && r.Lease == lease && r.SessionID == session && r.Authority == CoordinatorAbortAuthority {
			return r, true
		}
	}
	return LedgerRow{}, false
}
