package provider

import "fmt"

// PageDecision is the pagination control signal for multi-page listing.
// Termination is EMPTY-page based: a short-but-nonempty page must continue.
type PageDecision int

const (
	// PageContinue means more pages may exist; caller should request next page.
	PageContinue PageDecision = iota
	// PageStopEmpty means the page had zero items — definitive end of listing.
	PageStopEmpty
	// PageStopDuplicate means the page added no new IDs (server repeating a page).
	PageStopDuplicate
)

// DecidePagination chooses the next pagination action for a received page.
//
// Rules (fail-closed listing completeness):
//   - empty page (pageLen == 0) → stop; never treat short page as terminal
//   - page with only already-seen IDs (freshCount == 0) → stop (duplicate page)
//   - otherwise continue, even when pageLen < pageSize
func DecidePagination(pageLen, freshCount int) PageDecision {
	if pageLen == 0 {
		return PageStopEmpty
	}
	if freshCount == 0 {
		return PageStopDuplicate
	}
	return PageContinue
}

// PageAccumulator deduplicates items across pages by ID and reports how many
// fresh IDs each page contributed. Callers terminate via DecidePagination.
type PageAccumulator struct {
	seen map[string]struct{}
	ids  []string
}

// NewPageAccumulator returns an empty page accumulator.
func NewPageAccumulator() *PageAccumulator {
	return &PageAccumulator{seen: make(map[string]struct{})}
}

// Add records id if unseen. Returns true when the id is new.
func (a *PageAccumulator) Add(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := a.seen[id]; ok {
		return false
	}
	a.seen[id] = struct{}{}
	a.ids = append(a.ids, id)
	return true
}

// Len returns the number of unique IDs collected so far.
func (a *PageAccumulator) Len() int {
	return len(a.ids)
}

// IDs returns a copy of collected unique IDs in first-seen order.
func (a *PageAccumulator) IDs() []string {
	out := make([]string, len(a.ids))
	copy(out, a.ids)
	return out
}

// IngestPage adds ids from one page and returns (freshCount, decision).
func (a *PageAccumulator) IngestPage(ids []string) (fresh int, decision PageDecision) {
	if a.seen == nil {
		a.seen = make(map[string]struct{})
	}
	for _, id := range ids {
		if a.Add(id) {
			fresh++
		}
	}
	return fresh, DecidePagination(len(ids), fresh)
}

// ErrDuplicatePage is returned when a full page contributes zero new IDs and
// the caller elects to treat that as a hard anomaly rather than soft stop.
// Soft stop (PageStopDuplicate without error) is the default for listing.
var ErrDuplicatePage = fmt.Errorf("provider pagination: duplicate page with no new items")
