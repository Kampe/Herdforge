package standing

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	handoffTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:z|[+-]\d{2}:?\d{2})?\b`)
	handoffCounter   = regexp.MustCompile(`\b(?:counter|count|poll|iteration|tick|attempt)\s*[:=#]?\s*\d+\b`)
	parkedUntil      = regexp.MustCompile(`(?i)\bparked[\s_-]*until\s*[:=]?\s*([^\s;,]+)`)
	parkedMarker     = regexp.MustCompile(`(?i)\bparked[\s_-]*until\b`)
)

// HandoffObservation is the control-plane interpretation of one standing
// lane handoff. Progress is deliberately content-based: receiving a message
// is not evidence that the lane advanced.
type HandoffObservation struct {
	Lane            string
	Fingerprint     string
	Consecutive     int
	Progress        bool
	InformationFree bool
	Refocus         bool
	Parked          bool
}

type handoffState struct {
	fingerprint string
	consecutive int
	refocused   bool
	parked      bool
}

// HandoffTracker remembers the last semantic handoff for each lane. It is
// safe for a census and status reader to share one tracker.
type HandoffTracker struct {
	mu    sync.Mutex
	lanes map[string]handoffState
}

// NewHandoffTracker creates an empty per-lane handoff tracker.
func NewHandoffTracker() *HandoffTracker {
	return &HandoffTracker{lanes: make(map[string]handoffState)}
}

// Observe records one handoff. The first report and every changed report are
// progress. The first unchanged report is information-free and requests one
// refocus; further unchanged reports remain idle without producing a kick.
// An explicit parked-until declaration is terminal and never requests a kick.
func (t *HandoffTracker) Observe(lane, content string) HandoffObservation {
	if t == nil {
		t = NewHandoffTracker()
	}
	lane = strings.TrimSpace(lane)
	fingerprint, parked := NormalizeHandoff(content)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lanes == nil {
		t.lanes = make(map[string]handoffState)
	}
	previous, seen := t.lanes[lane]
	observation := HandoffObservation{Lane: lane, Fingerprint: fingerprint, Parked: parked}
	if parked {
		observation.InformationFree = true
		observation.Consecutive = 1
		if seen && previous.fingerprint == fingerprint {
			observation.Consecutive = previous.consecutive + 1
		}
		t.lanes[lane] = handoffState{fingerprint: fingerprint, consecutive: observation.Consecutive, parked: true}
		return observation
	}

	if !seen || previous.fingerprint != fingerprint {
		observation.Progress = true
		observation.Consecutive = 1
		t.lanes[lane] = handoffState{fingerprint: fingerprint, consecutive: 1}
		return observation
	}

	observation.InformationFree = true
	observation.Consecutive = previous.consecutive + 1
	if !previous.refocused {
		observation.Refocus = true
		previous.refocused = true
	}
	previous.consecutive = observation.Consecutive
	t.lanes[lane] = previous
	return observation
}

// NormalizeHandoff returns a semantic fingerprint and whether the report is
// an explicit parked-until terminal answer. Timestamps/counters are removed,
// while independent report lines and clauses are sorted so cosmetic ordering
// and polling changes cannot manufacture progress.
func NormalizeHandoff(content string) (fingerprint string, parked bool) {
	text := strings.TrimSpace(strings.ToLower(content))
	parkedValue := ""
	if match := parkedUntil.FindStringSubmatch(text); len(match) == 2 {
		parked = true
		parkedValue = strings.ToLower(match[1])
	}
	if parkedMarker.MatchString(text) {
		parked = true
	}
	if parked {
		text = parkedUntil.ReplaceAllString(text, " parked-until "+parkedValue)
		text = parkedMarker.ReplaceAllString(text, " parked-until ")
	}
	text = handoffTimestamp.ReplaceAllString(text, " ")
	text = handoffCounter.ReplaceAllString(text, " ")
	text = strings.NewReplacer("_", " ", "-", " ", "=", " ", ":", " ", ",", " ", ";", "\n").Replace(text)
	parts := strings.Fields(text)
	sort.Strings(parts)
	return strings.Join(parts, " "), parked
}
