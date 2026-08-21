package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FAC-553: board-audit reported 528 unclosed Done cards on this repository
// against 15 recorded receipts. A permanently-red count with 528
// undifferentiated rows is not a control: nobody reads it, so a NEW violation
// is invisible among the historical ones.
//
// The baseline records which cards were already unclosed when it was taken, so
// the audit can distinguish inherited damage from a fresh bypass and exit
// non-zero only on the latter. Then the actionable count starts at zero and any
// increase is a real regression.
//
// This deliberately records nothing about WHY a historical card is unclosed. It
// is not a receipt and must never be read as one: provenance that was never
// captured cannot be reconstructed, and inventing it would be the same class of
// mistake as the commit-subject oracle that caused the original damage.

// BaselineFile is the repository-relative baseline location.
const BaselineFile = ".herd/board-audit-baseline.json"

// AuditBaseline is the accepted set of already-unclosed Done cards.
type AuditBaseline struct {
	// CapturedAt is when an operator accepted this set.
	CapturedAt string `json:"captured_at"`
	// Actor is who accepted it, for attribution.
	Actor string `json:"actor"`
	// Note states plainly what the file is and is not.
	Note string `json:"note"`
	// TaskIDs are the provider task identities that were unclosed. Task id,
	// not ref: a re-minted ref is a different task and must not inherit
	// acceptance from the old one.
	TaskIDs []string `json:"task_ids"`
}

const baselineNote = "Cards already Done without a completion receipt when this baseline was accepted. " +
	"This is NOT evidence of completion and must never be read as a receipt: it only separates " +
	"inherited findings from new bypasses so a new one is visible."

// Has reports whether a task id was already unclosed at baseline time.
func (b *AuditBaseline) Has(taskID string) bool {
	if b == nil {
		return false
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false
	}
	for _, id := range b.TaskIDs {
		if id == taskID {
			return true
		}
	}
	return false
}

func baselinePath(repoDir string) string {
	if strings.TrimSpace(repoDir) == "" {
		repoDir = "."
	}
	return filepath.Join(repoDir, BaselineFile)
}

// ReadAuditBaseline loads the baseline. A missing file is not an error: it means
// no baseline has been accepted, so every finding counts as new.
func ReadAuditBaseline(repoDir string) (*AuditBaseline, error) {
	data, err := os.ReadFile(baselinePath(repoDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read board-audit baseline: %w", err)
	}
	var b AuditBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		// Fail closed. A corrupt baseline must not silently degrade to
		// "everything is historical", which would hide every new bypass.
		return nil, fmt.Errorf("parse board-audit baseline: %w", err)
	}
	return &b, nil
}

// WriteAuditBaseline records the given task ids as accepted-historical. It
// refuses to write an empty set, which would be a no-op file implying a clean
// board that was never verified.
func WriteAuditBaseline(repoDir, actor string, taskIDs []string) (*AuditBaseline, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("board-audit baseline: actor is required for attribution")
	}
	unique := map[string]bool{}
	for _, id := range taskIDs {
		if id = strings.TrimSpace(id); id != "" {
			unique[id] = true
		}
	}
	if len(unique) == 0 {
		return nil, fmt.Errorf("board-audit baseline: refusing to write an empty baseline")
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	b := &AuditBaseline{
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Actor:      actor,
		Note:       baselineNote,
		TaskIDs:    ids,
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode board-audit baseline: %w", err)
	}
	path := baselinePath(repoDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("board-audit baseline dir: %w", err)
	}
	// Write via a temp file in the same directory so a crash cannot leave a
	// truncated baseline, which would read as "these are all new".
	tmp, err := os.CreateTemp(filepath.Dir(path), ".board-audit-baseline-*")
	if err != nil {
		return nil, fmt.Errorf("board-audit baseline temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("board-audit baseline write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("board-audit baseline close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("board-audit baseline install: %w", err)
	}
	return b, nil
}

// PartitionFindings splits findings into new violations and inherited ones.
// An OVERRIDE finding is attributable by construction and is never a violation.
func PartitionFindings(findings []AuditFinding, baseline *AuditBaseline) (newViolations, historical []AuditFinding) {
	for _, f := range findings {
		if f.Kind == AuditOverride {
			historical = append(historical, f)
			continue
		}
		if baseline.Has(f.TaskID) {
			historical = append(historical, f)
			continue
		}
		newViolations = append(newViolations, f)
	}
	return newViolations, historical
}
