package reviewledger

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// TaskBindingOpts identifies one immutable verdict whose closeable-card task
// was recorded incorrectly. The prior verdict event digest is the anchor; a
// task name, SHA, or reviewer alone is never enough to authorize a correction.
type TaskBindingOpts struct {
	SHA              string
	Reviewer         string
	PreviousTask     string
	Task             string
	PriorEventDigest string
	Artifact         string
	ArtifactDigest   string
}

// BindTask appends a task-binding correction without rewriting any historical
// record or verdict. It is intentionally narrower than reassessment: no new
// verification is asserted, and the exact prior verdict event must be named.
func (l *Ledger) BindTask(opts TaskBindingOpts) error {
	if err := validateTaskBindingOpts(opts); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	release, err := lockVerdictMutation(l.Path)
	if err != nil {
		return err
	}
	defer release()
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	existing, err := taskBindingDecision(rows, opts)
	if err != nil {
		return err
	}
	if existing {
		return nil
	}
	row := &LedgerRow{
		Event:          string(EventTaskBinding),
		SHA:            opts.SHA,
		Reviewer:       opts.Reviewer,
		Task:           CloseableCardRef(opts.Task),
		PreviousTask:   CloseableCardRef(opts.PreviousTask),
		Reassesses:     strings.TrimSpace(opts.PriorEventDigest),
		Artifact:       opts.Artifact,
		ArtifactDigest: opts.ArtifactDigest,
		CandidateSHA:   opts.SHA,
		Reason:         "authenticated append-only correction of verdict task binding",
	}
	if err := l.appendRow(l.Path, row); err != nil {
		return err
	}
	check, err := readRows(l.Path)
	if err != nil {
		return fmt.Errorf("task binding readback: %w", err)
	}
	if len(check) == 0 || check[len(check)-1].Event != string(EventTaskBinding) || check[len(check)-1].Reassesses != row.Reassesses {
		return fmt.Errorf("task binding readback did not observe the appended event")
	}
	return nil
}

// CheckTaskBinding validates the same authenticated append-only correction as
// BindTask, but performs no mutation. It is the native dry-run/readiness path.
func (l *Ledger) CheckTaskBinding(opts TaskBindingOpts) (bool, error) {
	if err := validateTaskBindingOpts(opts); err != nil {
		return false, err
	}
	rows, err := readRows(l.Path)
	if err != nil {
		return false, err
	}
	return taskBindingDecision(rows, opts)
}

func validateTaskBindingOpts(opts TaskBindingOpts) error {
	if strings.TrimSpace(opts.SHA) == "" || strings.TrimSpace(opts.Reviewer) == "" ||
		strings.TrimSpace(opts.PriorEventDigest) == "" {
		return fmt.Errorf("task binding requires sha, reviewer, and prior event digest")
	}
	if len(strings.TrimSpace(opts.SHA)) != 40 {
		return fmt.Errorf("task binding requires the full candidate sha")
	}
	if _, err := hex.DecodeString(strings.TrimSpace(opts.SHA)); err != nil {
		return fmt.Errorf("task binding candidate sha is not hexadecimal")
	}
	if err := RequireCloseableCardRef(opts.PreviousTask, "previous task"); err != nil {
		return err
	}
	if err := RequireCloseableCardRef(opts.Task, "task"); err != nil {
		return err
	}
	if CloseableCardRef(opts.PreviousTask) == CloseableCardRef(opts.Task) {
		return fmt.Errorf("task binding must change the closeable card task")
	}
	if strings.TrimSpace(opts.Artifact) == "" || strings.TrimSpace(opts.ArtifactDigest) == "" {
		return fmt.Errorf("task binding requires the correction artifact and digest")
	}
	if len(strings.TrimSpace(opts.ArtifactDigest)) != 64 {
		return fmt.Errorf("task binding artifact digest must be a full SHA-256")
	}
	if _, err := hex.DecodeString(strings.TrimSpace(opts.ArtifactDigest)); err != nil {
		return fmt.Errorf("task binding artifact digest is not hexadecimal")
	}
	return nil
}

func taskBindingDecision(rows []LedgerRow, opts TaskBindingOpts) (bool, error) {
	var prior *LedgerRow
	for i := range rows {
		r := &rows[i]
		if r.SHA == opts.SHA && r.Reviewer == opts.Reviewer && r.Event == string(EventVerdict) {
			copy := *r
			prior = &copy
		}
	}
	if prior == nil {
		return false, fmt.Errorf("task binding prior verdict not found")
	}
	task := CloseableCardRef(prior.Task)
	anchor := VerdictEventDigest(*prior)
	if anchor == "" {
		return false, fmt.Errorf("task binding prior verdict has no stable event digest")
	}
	if CloseableCardRef(opts.PreviousTask) != task && strings.TrimSpace(opts.PriorEventDigest) == anchor {
		return false, fmt.Errorf("task binding previous task does not match the current chain task")
	}
	requestedDigest := strings.TrimSpace(opts.PriorEventDigest)
	for _, row := range rows {
		if row.Event != string(EventTaskBinding) || row.SHA != opts.SHA || row.Reviewer != opts.Reviewer {
			continue
		}
		if row.Reassesses != anchor || CloseableCardRef(row.PreviousTask) != task || CloseableCardRef(row.Task) == "" {
			return false, fmt.Errorf("invalid task binding chain for sha %s reviewer %q", opts.SHA, opts.Reviewer)
		}
		if row.Reassesses == requestedDigest && row.PreviousTask == CloseableCardRef(opts.PreviousTask) && row.Task == CloseableCardRef(opts.Task) && row.ArtifactDigest == opts.ArtifactDigest {
			return true, nil
		}
		task = CloseableCardRef(row.Task)
		anchor = VerdictEventDigest(row)
	}
	if requestedDigest != anchor || CloseableCardRef(opts.PreviousTask) != task {
		return false, fmt.Errorf("task binding prior event digest is stale or unbound")
	}
	return false, nil
}

// EffectiveTask applies only valid, chained task-binding events to a verdict.
// Raw rows remain available through AllRows for audit and historical retention.
func EffectiveTask(rows []LedgerRow, verdict LedgerRow) string {
	task := verdict.Task
	anchor := VerdictEventDigest(verdict)
	for _, row := range rows {
		if row.Event != string(EventTaskBinding) || row.SHA != verdict.SHA || row.Reviewer != verdict.Reviewer ||
			row.Reassesses != anchor || row.PreviousTask != task || CloseableCardRef(row.Task) == "" {
			continue
		}
		task = row.Task
		anchor = VerdictEventDigest(row)
	}
	return task
}
