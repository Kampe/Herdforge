package reviewledger

import (
	"errors"
	"fmt"
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
	if v.Task != task || prior.Task != task || v.CandidateSHA != sha {
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
	if prior.BuilderFamily != "" && prior.BuilderFamily != FamilyUnrecorded && prior.BuilderFamily != v.BuilderFamily {
		return fmt.Errorf("conflicting recorded builder family")
	}
	if prior.ReviewerFamily != "" && prior.ReviewerFamily != v.ReviewerFamily {
		return fmt.Errorf("conflicting recorded reviewer family")
	}
	if prior.Branch != "" && prior.Branch != evidence.Branch {
		return fmt.Errorf("conflicting recorded branch")
	}
	if prior.Tier != "" && prior.Tier != evidence.Tier {
		return fmt.Errorf("conflicting recorded risk tier")
	}
	if prior.BuilderFamily == v.BuilderFamily && prior.ReviewerFamily == v.ReviewerFamily && prior.Tier == evidence.Tier && prior.Branch == evidence.Branch && prior.Gate == "independent" {
		return nil
	}
	completed := *prior
	completed.BuilderFamily = v.BuilderFamily
	completed.ReviewerFamily = v.ReviewerFamily
	completed.Tier = evidence.Tier
	completed.Branch = evidence.Branch
	completed.Gate = "independent"
	completed.Reason = "completed from retained admitted artifact and native launch evidence"
	return l.appendRow(l.Path, &completed)
}
