package invariant

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// BaselineFile is the accepted set of pre-existing duplicated rules.
//
// FAC-575: the tree already has hundreds. A gate that is red on day one is
// ignored by day two -- that is exactly how board-audit accumulated 528
// unread findings (FAC-553). So today's duplicates are recorded as inherited and
// the gate fails only on NEW ones. The actionable count starts at zero and any
// increase is a real regression.
//
// This is NOT an approval of the inherited entries. It is a starting line.
const BaselineFile = ".herd/duplicate-rule-baseline.json"

// Baseline is the inherited set, keyed by literal to the files it appeared in.
type Baseline struct {
	CapturedAt string              `json:"captured_at"`
	Note       string              `json:"note"`
	Inherited  map[string][]string `json:"inherited"`
}

const baselineNote = "Duplicated distinctive literals present when this gate was introduced. " +
	"NOT an approval: each is a rule written down more than once and a candidate for " +
	"consolidation. The gate fails only on NEW duplicates so a regression is visible."

// LoadBaseline reads the baseline. A missing file means nothing is inherited, so
// every duplicate counts as new.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Baseline{Inherited: map[string][]string{}}, nil
		}
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		// Fail closed: a corrupt baseline must not read as "all inherited".
		return nil, fmt.Errorf("parse duplicate-rule baseline: %w", err)
	}
	if b.Inherited == nil {
		b.Inherited = map[string][]string{}
	}
	return &b, nil
}

// NewViolations returns occurrences that are not inherited.
//
// A literal already in the baseline but now in MORE files is a new violation:
// the rule spread further, which is the regression this gate exists to catch.
func NewViolations(found []Occurrence, base *Baseline) []Occurrence {
	var out []Occurrence
	for _, occ := range found {
		known, ok := base.Inherited[occ.Literal]
		if !ok {
			out = append(out, occ)
			continue
		}
		knownSet := map[string]bool{}
		for _, f := range known {
			knownSet[f] = true
		}
		var added []string
		for _, f := range occ.Files {
			if !knownSet[f] {
				added = append(added, f)
			}
		}
		if len(added) > 0 {
			out = append(out, Occurrence{Literal: occ.Literal, Files: added})
		}
	}
	return out
}

// WriteBaseline records the given occurrences as inherited.
func WriteBaseline(path, capturedAt string, found []Occurrence) error {
	b := Baseline{CapturedAt: capturedAt, Note: baselineNote, Inherited: map[string][]string{}}
	for _, occ := range found {
		files := append([]string(nil), occ.Files...)
		sort.Strings(files)
		b.Inherited[occ.Literal] = files
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Describe renders a violation for a failing gate, naming every file so the
// duplicate is fixable without a second investigation.
func Describe(occ Occurrence) string {
	return fmt.Sprintf("  %q\n    in: %s", occ.Literal, strings.Join(occ.Files, ", "))
}
