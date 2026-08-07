package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-132: report Done cards that no receipt ever closed. Read-only by
// construction — this file has no provider write call and no git write.
//
// The historical damage (FAC-107, FAC-108, FAC-111, FAC-114, FAC-116,
// FAC-128, FAC-129) was done by an oracle that accepted commit-subject
// matches. Those cards are still Done. Re-opening them automatically would be
// the same class of mistake in reverse — a machine deciding a human's
// acceptance criteria. So this reports, and stops.

// Audit finding kinds, most to least suspicious.
const (
	AuditNoEvidence     = "NO_EVIDENCE"      // Done, no record, nothing on origin/main names it
	AuditCommitHintOnly = "COMMIT_HINT_ONLY" // Done, no record, only a commit-subject match
	AuditOverride       = "OVERRIDE"         // Done by attributable manual override
)

// AuditFinding is one suspicious Done card. Detail is diagnostic text.
type AuditFinding struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// AuditDone reports Done cards not closed by a completion receipt. Cards with
// a recorded receipt produce no finding. It never mutates the board.
func AuditDone(ctx context.Context, tp provider.TaskProvider, repoDir, projectID string) ([]AuditFinding, error) {
	if tp == nil {
		return nil, fmt.Errorf("task provider is nil")
	}
	if repoDir == "" {
		repoDir = "."
	}

	tasks, err := tp.ListTasks(ctx, projectID, "")
	if err != nil {
		// Fail closed: a provider timeout must never read as "no suspicious cards".
		return nil, boardCallErr("list tasks for done audit", err)
	}
	log, err := ReadDoneLog(repoDir)
	if err != nil {
		return nil, err
	}

	// One refresh for the whole sweep. Offline is fine — the hints below read
	// the local origin/main, and a stale hint only affects diagnostic text,
	// never a decision (nothing here decides anything).
	_, _ = git(repoDir, "fetch", "-q", "origin", "main")
	haveOriginMain := true
	if _, err := git(repoDir, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		haveOriginMain = false
	}

	// Index closures by provider task id — the task binding, not the ref,
	// since a re-minted ref is a different task.
	byTask := make(map[string][]DoneRecord)
	for _, rec := range log {
		byTask[rec.TaskID] = append(byTask[rec.TaskID], rec)
	}

	var findings []AuditFinding
	for _, t := range tasks {
		if t.Status != "done" || t.Ref == "" {
			continue
		}
		ref := NormalizeRef(t.Ref)
		var latestOverride *OverrideRecord
		closedByReceipt := false
		for _, rec := range byTask[t.ID] {
			if rec.ReceiptDigest != "" {
				closedByReceipt = true
				break
			}
			if rec.Override != nil {
				latestOverride = rec.Override
			}
		}
		if closedByReceipt {
			continue
		}
		if latestOverride != nil {
			findings = append(findings, AuditFinding{
				Kind: AuditOverride, Ref: ref, TaskID: t.ID, Title: t.Title,
				Detail: fmt.Sprintf("closed by %s under policy %s: %s", latestOverride.Actor, latestOverride.Policy, latestOverride.Reason),
			})
			continue
		}

		// No durable record at all. Distinguish "a commit mentions it" from
		// "nothing anywhere does" — the first is exactly the oracle that
		// produced the incident, so name it as a hint, never as evidence.
		hint := ""
		if haveOriginMain {
			hint = commitHint(repoDir, ref)
		}
		switch {
		case !haveOriginMain:
			findings = append(findings, AuditFinding{
				Kind: AuditNoEvidence, Ref: ref, TaskID: t.ID, Title: t.Title,
				Detail: "no closure record; origin/main could not be inspected",
			})
		case strings.TrimSpace(hint) != "":
			findings = append(findings, AuditFinding{
				Kind: AuditCommitHintOnly, Ref: ref, TaskID: t.ID, Title: t.Title,
				Detail: "no closure record; only a commit-subject hint: " + hint,
			})
		default:
			findings = append(findings, AuditFinding{
				Kind: AuditNoEvidence, Ref: ref, TaskID: t.ID, Title: t.Title,
				Detail: "no closure record and no commit on origin/main names it",
			})
		}
	}
	return findings, nil
}
