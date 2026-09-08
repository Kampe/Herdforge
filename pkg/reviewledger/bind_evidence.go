package reviewledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/verifier"
)

// BindEvidence appends EventEvidenceBind for exactly one eligible independent
// PASS. It never rewrites historical rows, never enqueues harvest work, and
// is not completion authority.
func (l *Ledger) BindEvidence(task, sha, digest string, receipt verifier.Receipt) error {
	task = strings.TrimSpace(task)
	sha = strings.TrimSpace(sha)
	digest = strings.TrimSpace(digest)
	if err := validateBindableReceipt(task, sha, digest, receipt); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	before, err := os.ReadFile(l.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	rows, err := readRows(l.Path)
	if err != nil {
		return err
	}

	passes, contradict, err := independentPassTargets(l, rows, sha, task)
	if err != nil {
		return err
	}
	if contradict {
		return fmt.Errorf("contradictory review evidence for %s candidate %s", task, sha)
	}
	if len(passes) == 0 {
		return fmt.Errorf("no independent PASS verdict for %s candidate %s", task, sha)
	}
	if len(passes) != 1 {
		return fmt.Errorf("ambiguous independent PASS evidence for %s candidate %s", task, sha)
	}
	target := passes[0]
	existing := strings.TrimSpace(target.VerificationDigest)
	if existing == "" {
		existing = lastBoundDigest(rows, sha, target.Reviewer, task)
	} else if bound := lastBoundDigest(rows, sha, target.Reviewer, task); bound != "" && bound != existing {
		return fmt.Errorf("contradictory verification digest binding for %s candidate %s", task, sha)
	}
	if existing != "" {
		if existing == digest {
			return nil
		}
		return fmt.Errorf("conflicting verification binding for %s candidate %s", task, sha)
	}

	if err := l.appendRow(l.Path, &LedgerRow{
		Event:              string(EventEvidenceBind),
		SHA:                sha,
		Task:               task,
		Reviewer:           target.Reviewer,
		VerificationDigest: digest,
		CandidateSHA:       sha,
		Lease:              receipt.LeaseGeneration,
		Status:             "bound",
		Reason:             "bound immutable full-suite PASS receipt",
	}); err != nil {
		return err
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		return err
	}
	if len(before) > 0 && !strings.HasPrefix(string(after), string(before)) {
		return fmt.Errorf("evidence bind rewrote historical ledger bytes")
	}
	return nil
}

func validateBindableReceipt(task, sha, digest string, receipt verifier.Receipt) error {
	if task == "" {
		return fmt.Errorf("task ref is required")
	}
	if sha == "" {
		return fmt.Errorf("candidate sha is required")
	}
	if digest == "" {
		return fmt.Errorf("receipt digest is required")
	}
	if err := receipt.ValidateDigest(); err != nil {
		return fmt.Errorf("receipt digest is invalid: %w", err)
	}
	if receipt.Digest != digest {
		return fmt.Errorf("receipt digest does not match requested digest")
	}
	if !strings.EqualFold(strings.TrimSpace(receipt.TaskRef), task) {
		return fmt.Errorf("receipt task %q does not match %s", receipt.TaskRef, task)
	}
	if strings.TrimSpace(receipt.CandidateSHA) != sha {
		return fmt.Errorf("receipt candidate %s does not match %s", receipt.CandidateSHA, sha)
	}
	if receipt.Outcome != verifier.OutcomePASS {
		return fmt.Errorf("receipt outcome %s is not PASS", receipt.Outcome)
	}
	if !fullSuiteTestArgv(receipt.Command) {
		return fmt.Errorf("receipt command is not the full-suite test profile")
	}
	return nil
}

func fullSuiteTestArgv(argv []string) bool {
	if len(argv) < 3 {
		return false
	}
	if filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return false
	}
	hasSuite := false
	for _, a := range argv[2:] {
		if a == "./..." {
			hasSuite = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.HasPrefix(a, "./") && a != "./..." {
			return false
		}
	}
	return hasSuite
}

func independentPassTargets(l *Ledger, rows []LedgerRow, sha, task string) (passes []LedgerRow, contradict bool, err error) {
	launch := map[projectionKey]LedgerRow{}
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha {
			launch[rowProjection(r)] = r
		}
	}
	latest := map[projectionKey]LedgerRow{}
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == sha {
			latest[rowProjection(r)] = r
		}
	}
	var veto bool
	for k, verdict := range latest {
		reviewer := k.Reviewer
		if strings.TrimSpace(verdict.Task) != "" && !strings.EqualFold(verdict.Task, task) {
			continue
		}
		if verdict.Verdict == string(VerdictFAIL) || verdict.Verdict == string(VerdictBLOCKED) {
			if !l.isCoordinator(reviewer) {
				if launchRow, ok := launch[k]; ok && launchRow.BuilderFamily != "" && FamilyAllowlist[launchRow.BuilderFamily] {
					veto = true
				}
			}
			continue
		}
		if verdict.Verdict != string(VerdictPASS) {
			continue
		}
		if l.isCoordinator(reviewer) {
			continue
		}
		launchRow, ok := launch[k]
		if !ok {
			continue
		}
		if launchRow.BuilderFamily == "" || !FamilyAllowlist[launchRow.BuilderFamily] {
			continue
		}
		families := resolveFamily(launchRow.BuilderFamily, launchRow.ReviewerFamily, verdict.BuilderFamily, verdict.ReviewerFamily)
		if families.State != familySet || families.Value == launchRow.BuilderFamily || !FamilyAllowlist[families.Value] {
			continue
		}
		if verdict.Reviewer == "" {
			continue
		}
		if launchRow.BuilderIdentity != "" && verdict.Reviewer == launchRow.BuilderIdentity {
			continue
		}
		if strings.TrimSpace(verdict.Task) != "" && !strings.EqualFold(verdict.Task, task) {
			continue
		}
		passes = append(passes, verdict)
	}
	return passes, veto && len(passes) > 0, nil
}

func lastBoundDigest(rows []LedgerRow, sha, reviewer, task string) string {
	digest := ""
	for _, r := range rows {
		if r.Event != string(EventEvidenceBind) || r.SHA != sha || r.Reviewer != reviewer {
			continue
		}
		if strings.TrimSpace(r.Task) != "" && !strings.EqualFold(r.Task, task) {
			continue
		}
		if d := strings.TrimSpace(r.VerificationDigest); d != "" {
			digest = d
		}
	}
	return digest
}

func verificationDigestFor(rows []LedgerRow, verdict LedgerRow) (string, error) {
	onRow := strings.TrimSpace(verdict.VerificationDigest)
	bound := lastBoundDigest(rows, verdict.SHA, verdict.Reviewer, verdict.Task)
	if onRow != "" && bound != "" && onRow != bound {
		return "", fmt.Errorf("contradictory verification digest binding")
	}
	if onRow != "" {
		return onRow, nil
	}
	return bound, nil
}
