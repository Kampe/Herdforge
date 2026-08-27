package reviewledger

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FAC-578 multi-ref decision: one verdict carries exactly one closeable card
// ref (PREFIX-NUMBER). A lane branch covering N cards needs N reviews (or N
// artifacts). Multi-ref in one artifact is refused until a first-class schema
// exists; silence is what produced the 253-card accounting leak where reviews
// were recorded against standing/* and wt/* names that board-done can never
// close.
//
// exactCloseableCardRefRe matches the ENTIRE string. Substring harvest (for
// example taking CHA-2120 out of fix/cha-2120-telegram) would misattribute a
// branch location as a closable card.
var exactCloseableCardRefRe = regexp.MustCompile(`(?i)^([a-z]{2,6})-([0-9]{1,6})$`)

// CloseableCardRef returns the canonical PREFIX-NUMBER form when v is exactly
// one closable board card ref. Anything else, including a branch name that
// merely contains a card-shaped token, returns "".
func CloseableCardRef(v string) string {
	m := exactCloseableCardRefRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + "-" + m[2]
}

// IsCloseableCardRef reports whether v is exactly one closable board card ref.
func IsCloseableCardRef(v string) bool {
	return CloseableCardRef(v) != ""
}

// RequireCloseableCardRef refuses a task identity board-done cannot consume.
func RequireCloseableCardRef(task, surface string) error {
	got := strings.TrimSpace(task)
	if CloseableCardRef(got) == "" {
		if surface == "" {
			surface = "task"
		}
		if got == "" {
			return fmt.Errorf("FAC-578: %s is empty; a review must name exactly one closeable card ref (PREFIX-NUMBER), not a branch", surface)
		}
		return fmt.Errorf("FAC-578: %s %q is not a closeable card ref; board-done cannot close a branch or free-form label (want PREFIX-NUMBER)", surface, got)
	}
	return nil
}

// NonCloseableTaskCount is one distinct non-card Task value found in the ledger.
type NonCloseableTaskCount struct {
	Task  string `json:"task"`
	Count int    `json:"count"`
}

// EvidenceGapReport makes the FAC-578 accounting leak visible.
//
// NonCloseableTasks lists ledger Task values that are not closeable card refs
// (the historical standing/* and wt/* leak). InReviewWithoutEvidence lists
// board in-review card refs that have no ledger row naming them, when the
// caller supplies the live in-review set.
type EvidenceGapReport struct {
	NonCloseableTasks       []NonCloseableTaskCount `json:"non_closeable_tasks"`
	InReviewWithoutEvidence []string                `json:"in_review_without_evidence"`
	InReviewChecked         int                     `json:"in_review_checked"`
	LedgerRowsScanned       int                     `json:"ledger_rows_scanned"`
}

// BuildEvidenceGapReport scans ledger rows for non-closeable Task values and,
// when inReviewRefs is non-nil, lists in-review cards with no ledger evidence
// under that exact card ref.
func BuildEvidenceGapReport(rows []LedgerRow, inReviewRefs []string) EvidenceGapReport {
	counts := map[string]int{}
	evidence := map[string]struct{}{}
	for _, row := range rows {
		task := strings.TrimSpace(row.Task)
		if task == "" {
			continue
		}
		if card := CloseableCardRef(task); card != "" {
			evidence[card] = struct{}{}
			continue
		}
		counts[task]++
	}

	non := make([]NonCloseableTaskCount, 0, len(counts))
	for task, n := range counts {
		non = append(non, NonCloseableTaskCount{Task: task, Count: n})
	}
	sort.Slice(non, func(i, j int) bool {
		if non[i].Count != non[j].Count {
			return non[i].Count > non[j].Count
		}
		return non[i].Task < non[j].Task
	})

	report := EvidenceGapReport{
		NonCloseableTasks: non,
		LedgerRowsScanned: len(rows),
	}
	if inReviewRefs == nil {
		return report
	}
	report.InReviewChecked = len(inReviewRefs)
	missing := make([]string, 0)
	seen := map[string]struct{}{}
	for _, ref := range inReviewRefs {
		card := CloseableCardRef(ref)
		if card == "" {
			continue
		}
		if _, ok := evidence[card]; ok {
			continue
		}
		if _, dup := seen[card]; dup {
			continue
		}
		seen[card] = struct{}{}
		missing = append(missing, card)
	}
	sort.Strings(missing)
	report.InReviewWithoutEvidence = missing
	return report
}
