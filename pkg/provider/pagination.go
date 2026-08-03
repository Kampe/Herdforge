package provider

import "fmt"

// DefaultMaxListPages is the safety ceiling for multi-page listing loops.
// Hitting this cap without observing an empty page is a hard error.
const DefaultMaxListPages = 50

// PageDecision is the pagination control signal for multi-page listing.
// Termination is EMPTY-page based: a short-but-nonempty page must continue.
// Successful listing requires PageStopEmpty; PageStopDuplicate and the page
// cap are hard errors (incomplete enumeration is not success).
type PageDecision int

const (
	// PageContinue means more pages may exist; caller should request next page.
	PageContinue PageDecision = iota
	// PageStopEmpty means the page had zero items — definitive end of listing.
	PageStopEmpty
	// PageStopDuplicate means the page added no new IDs (server repeating a page).
	// Callers must treat this as ErrDuplicatePage, not a successful soft stop.
	PageStopDuplicate
)

// DecidePagination chooses the next pagination action for a received page.
//
// Rules (fail-closed listing completeness):
//   - empty page (pageLen == 0) → PageStopEmpty (only successful termination)
//   - page with only already-seen IDs (freshCount == 0) → PageStopDuplicate (hard error)
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

// PaginationTerminalError maps a page decision to a hard error when listing
// must not succeed. PageStopEmpty returns nil (success). PageStopDuplicate
// returns ErrDuplicatePage. PageContinue returns nil so the caller loops.
func PaginationTerminalError(d PageDecision) error {
	switch d {
	case PageStopEmpty:
		return nil
	case PageStopDuplicate:
		return ErrDuplicatePage
	default:
		return nil
	}
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

// ErrDuplicatePage is returned when a non-empty page contributes zero new IDs
// without empty-page termination — incomplete listing, fail closed.
var ErrDuplicatePage = fmt.Errorf("provider pagination: non-empty duplicate page without empty-page termination")

// ErrPaginationCap is returned when DefaultMaxListPages (or the caller's cap)
// is exhausted without observing an empty page.
var ErrPaginationCap = fmt.Errorf("provider pagination: page cap reached without empty-page termination")
