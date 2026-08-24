package reviewledger

import "strings"

// ProvenBuilderFamily returns the allowlisted builder family recorded for an
// exact candidate SHA, or "" when none is provable.
//
// FAC-608: admission refuses a verdict whose builder-family is not provable, and
// it does so at INGEST -- after a review lane, an hour of wall clock and a slice
// of quota have already been spent. 25 of 41 refusals in one inbox were exactly
// this, and the reviewers were being honest: the packet never told them the
// builder family, so they wrote "unknown", and "unknown" is refused. Meanwhile
// any allowlisted string is admitted with no verification, so the gate punished
// honesty and rewarded assertion.
//
// The same question is answerable before dispatch for free. Exported so the
// dispatch path can decline a candidate it could never admit, and so the review
// packet can PREFILL the family instead of asking a reviewer to guess it.
func (l *Ledger) ProvenBuilderFamily(sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" || l == nil {
		return "", nil
	}
	rows, err := readRows(l.Path)
	if err != nil {
		// Fail closed: an unreadable ledger is not proof of an unprovable
		// candidate, and it must not be reported as one.
		return "", err
	}
	for _, r := range rows {
		if r.Event != string(EventRecord) || r.SHA != sha {
			continue
		}
		family := strings.TrimSpace(r.BuilderFamily)
		if family != "" && FamilyAllowlist[family] {
			return family, nil
		}
	}
	return "", nil
}
