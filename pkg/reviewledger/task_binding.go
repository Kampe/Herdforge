package reviewledger

import (
	"encoding/hex"
	"fmt"
	"os"
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
	var prior *LedgerRow
	for i := range rows {
		r := &rows[i]
		if r.SHA == opts.SHA && r.Reviewer == opts.Reviewer && r.Event == string(EventVerdict) {
			copy := *r
			prior = &copy
		}
	}
	if prior == nil {
		return fmt.Errorf("task binding prior verdict not found")
	}
	if VerdictEventDigest(*prior) != strings.TrimSpace(opts.PriorEventDigest) {
		return fmt.Errorf("task binding prior event digest is stale or unbound")
	}
	if CloseableCardRef(prior.Task) != CloseableCardRef(opts.PreviousTask) {
		return fmt.Errorf("task binding previous task does not match the prior verdict")
	}
	for _, row := range rows {
		if row.Event != string(EventTaskBinding) || row.SHA != opts.SHA || row.Reviewer != opts.Reviewer {
			continue
		}
		if row.Reassesses == opts.PriorEventDigest && row.PreviousTask == opts.PreviousTask && row.Task == opts.Task && row.ArtifactDigest == opts.ArtifactDigest {
			return nil
		}
		return fmt.Errorf("conflicting task binding already exists for sha %s reviewer %q", opts.SHA, opts.Reviewer)
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
	if _, err := os.Stat(l.Path); err != nil {
		return fmt.Errorf("task binding readback: %w", err)
	}
	return nil
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
