package reviewledger

import (
	"errors"
	"fmt"
	"strings"
)

// RecordCompletion contains only deterministic metadata; family and identity
// are inherited from the admitted verdict, never supplied by the operator.
type RecordCompletion struct{ Branch, Tier string }

// CompleteAdmissionRecord repairs one incomplete launch record. The verifier
// must validate the retained artifact and its native launch receipt against
// the current verdict. It runs inside the same inode lock as compare/append,
// so a concurrent reassessment cannot change the consent being completed.
func (l *Ledger) CompleteAdmissionRecord(task, sha, reviewer string, verify func(LedgerRow) (RecordCompletion, error)) (err error) {
	if verify == nil {
		return fmt.Errorf("record completion requires retained evidence verification")
	}
	if err := RequireCloseableCardRef(task, "record completion task"); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	release, err := lockVerdictMutation(l.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	var prior *LedgerRow
	latest := map[projectionKey]LedgerRow{}
	for i := range rows {
		r := rows[i]
		if r.SHA != sha {
			continue
		}
		if r.Event == string(EventRecord) && r.Reviewer == reviewer {
			copy := r
			prior = &copy
		}
		if r.Event == string(EventVerdict) {
			latest[rowProjection(r)] = r
		}
		if r.Event == string(EventRetired) {
			return fmt.Errorf("retired candidate cannot complete admission")
		}
	}
	var v LedgerRow
	found := false
	for k, row := range latest {
		if k.Reviewer == reviewer && row.Verdict == string(VerdictPASS) {
			v = row
			found = true
		}
	}
	if !found || prior == nil || l.isCoordinator(reviewer) {
		return fmt.Errorf("exact independent PASS and launch record required")
	}
	for k, r := range latest {
		if !l.isCoordinator(k.Reviewer) && (r.Verdict == string(VerdictFAIL) || r.Verdict == string(VerdictBLOCKED)) {
			return fmt.Errorf("candidate has review dissent")
		}
	}
	if v.Task != task || v.CandidateSHA != sha {
		return fmt.Errorf("record completion task/candidate binding differs")
	}
	if !FamilyAllowlist[v.BuilderFamily] || !FamilyAllowlist[v.ReviewerFamily] || v.BuilderFamily == v.ReviewerFamily || prior.BuilderIdentity == reviewer {
		return fmt.Errorf("record completion requires independent admitted families")
	}
	if v.ArtifactDigest == "" || v.VerificationDigest == "" {
		return fmt.Errorf("admitted artifact and verification digests required")
	}
	evidence, err := verify(v)
	if err != nil {
		return err
	}
	switch evidence.Tier {
	case "R0", "R1", "R2", "R3":
	default:
		return fmt.Errorf("deterministic risk tier required")
	}
	if evidence.Branch == "" {
		return fmt.Errorf("retained branch required")
	}
	want := RecordOpts{
		SHA: sha, Reviewer: reviewer, Task: task, Branch: evidence.Branch, Tier: evidence.Tier,
		BuilderFamily: v.BuilderFamily, ReviewerFamily: v.ReviewerFamily, Gate: "independent",
		Artifact: v.Artifact,
	}
	completed, err := mergeAuthenticatedLaunchRecord(*prior, want)
	if err != nil {
		return err
	}
	if launchRecordComplete(*prior, completed) {
		return nil
	}
	completed.Reason = "completed from retained admitted artifact and native launch evidence"
	return l.appendRow(l.Path, &completed)
}

// bindCloseableRecordTask binds a prior launch Task to the authenticated
// closeable card. A wrong closeable identity is always refused. A legacy
// non-closeable Task is accepted only when it equals the verified artifact
// branch. Shared by ingest and review-complete-record.
func bindCloseableRecordTask(priorTask, closeableTask, verifiedBranch string) (string, error) {
	if err := RequireCloseableCardRef(closeableTask, "record completion task"); err != nil {
		return "", err
	}
	want := CloseableCardRef(closeableTask)
	if prior := CloseableCardRef(priorTask); prior != "" {
		if prior != want {
			return "", fmt.Errorf("record completion task/candidate binding differs")
		}
		return want, nil
	}
	if strings.TrimSpace(priorTask) == "" || strings.TrimSpace(verifiedBranch) == "" || priorTask != verifiedBranch {
		return "", fmt.Errorf("record completion task/candidate binding differs")
	}
	return want, nil
}

func mergeAuthenticatedLaunchRecord(prior LedgerRow, want RecordOpts) (LedgerRow, error) {
	boundTask, err := bindCloseableRecordTask(prior.Task, want.Task, want.Branch)
	if err != nil {
		return LedgerRow{}, err
	}
	if strings.TrimSpace(want.Branch) == "" {
		return LedgerRow{}, fmt.Errorf("retained branch required")
	}
	switch want.Tier {
	case "R0", "R1", "R2", "R3":
	default:
		return LedgerRow{}, fmt.Errorf("deterministic risk tier required")
	}
	if want.BuilderFamily == "" || want.BuilderFamily == FamilyUnrecorded || !FamilyAllowlist[want.BuilderFamily] {
		return LedgerRow{}, fmt.Errorf("record completion requires independent admitted families")
	}
	if want.ReviewerFamily == "" || !FamilyAllowlist[want.ReviewerFamily] || want.BuilderFamily == want.ReviewerFamily {
		return LedgerRow{}, fmt.Errorf("record completion requires independent admitted families")
	}
	if prior.BuilderFamily != "" && prior.BuilderFamily != FamilyUnrecorded && prior.BuilderFamily != want.BuilderFamily {
		return LedgerRow{}, fmt.Errorf("conflicting recorded builder family")
	}
	if prior.ReviewerFamily != "" && prior.ReviewerFamily != want.ReviewerFamily {
		return LedgerRow{}, fmt.Errorf("conflicting recorded reviewer family")
	}
	if prior.Branch != "" && prior.Branch != want.Branch {
		return LedgerRow{}, fmt.Errorf("conflicting recorded branch")
	}
	if prior.Tier != "" && prior.Tier != want.Tier {
		return LedgerRow{}, fmt.Errorf("conflicting recorded risk tier")
	}
	completed := prior
	completed.Task = boundTask
	completed.Branch = want.Branch
	completed.Tier = want.Tier
	completed.BuilderFamily = want.BuilderFamily
	completed.ReviewerFamily = want.ReviewerFamily
	completed.Gate = "independent"
	if strings.TrimSpace(want.Artifact) != "" {
		completed.Artifact = want.Artifact
	}
	return completed, nil
}

func launchRecordComplete(prior, want LedgerRow) bool {
	return prior.Task == want.Task &&
		prior.BuilderFamily == want.BuilderFamily &&
		prior.ReviewerFamily == want.ReviewerFamily &&
		prior.Tier == want.Tier &&
		prior.Branch == want.Branch &&
		prior.Gate == "independent"
}

func (l *Ledger) completeAuthenticatedLaunchRecord(want RecordOpts) error {
	if want.Gate != "independent" {
		return nil
	}
	if strings.TrimSpace(want.Tier) == "" {
		return nil
	}
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}
	var prior *LedgerRow
	for i := range rows {
		if rows[i].Event == string(EventRecord) && rows[i].SHA == want.SHA && rows[i].Reviewer == want.Reviewer {
			copy := rows[i]
			prior = &copy
		}
	}
	if prior == nil {
		return nil
	}
	completed, err := mergeAuthenticatedLaunchRecord(*prior, want)
	if err != nil {
		return err
	}
	if launchRecordComplete(*prior, completed) {
		return nil
	}
	completed.Reason = "completed from retained admitted artifact and native launch evidence"
	return l.appendRow(l.Path, &completed)
}
