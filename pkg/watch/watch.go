// Package watch ports bin/herd-watch: an event-ish settle detector for the
// coordinator.
//
// The coordinator must harvest the moment a builder or reviewer leaves
// `working`. Three properties carry that, and each exists because its absence
// cost real time in chainseer:
//
//   - A SHORT interval. The default was 60s, which alone added multi-minute
//     lag to every harvest.
//   - RE-ENUMERATION every poll. Watching a fixed pane list drops reviewers
//     that were spawned mid-wave, so they settle unnoticed.
//   - DEBOUNCE. Agent status flaps (observed on agy), so a single non-working
//     poll is not a settle; two consecutive ones are.
package watch

import (
	"strings"
	"time"
)

// DefaultInterval is deliberately short: harvest lag is the cost being paid.
const DefaultInterval = 5 * time.Second

// DebouncePolls is how many consecutive non-working polls make a settle.
// One is not enough — status flapping would fire a false harvest.
const DebouncePolls = 2

// Settled reports whether a status counts as "no longer working".
func Settled(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "starting":
		return false
	}
	return true
}

// State tracks debounce counts per pane across polls.
type State struct {
	nonWorking map[string]int
	fired      map[string]bool
}

func NewState() *State {
	return &State{nonWorking: map[string]int{}, fired: map[string]bool{}}
}

// Observation is one pane seen in a poll.
type Observation struct {
	PaneID string
	Name   string
	Status string
}

// Event is a confirmed settle.
type Event struct {
	PaneID string
	Name   string
	Status string
}

// Poll folds one round of observations into the debounce state and returns
// newly confirmed settles.
//
// Panes are supplied fresh every call by the caller — that IS the
// re-enumeration property; a caller that passes a fixed list will silently
// miss mid-wave agents.
func (s *State) Poll(obs []Observation) []Event {
	var events []Event
	seen := map[string]bool{}

	for _, o := range obs {
		seen[o.PaneID] = true
		if !Settled(o.Status) {
			// Back to work: clear the debounce AND the fired latch, so a pane
			// that settles, gets more work, then settles again fires twice.
			s.nonWorking[o.PaneID] = 0
			s.fired[o.PaneID] = false
			continue
		}
		s.nonWorking[o.PaneID]++
		if s.nonWorking[o.PaneID] >= DebouncePolls && !s.fired[o.PaneID] {
			s.fired[o.PaneID] = true
			events = append(events, Event{PaneID: o.PaneID, Name: o.Name, Status: o.Status})
		}
	}

	// A pane that vanished between polls (tab closed) must not keep stale
	// debounce state that would fire the instant an ID is reused.
	for id := range s.nonWorking {
		if !seen[id] {
			delete(s.nonWorking, id)
			delete(s.fired, id)
		}
	}
	return events
}

// AllSettled reports whether every named pane has fired.
func (s *State) AllSettled(paneIDs []string) bool {
	if len(paneIDs) == 0 {
		return false
	}
	for _, id := range paneIDs {
		if !s.fired[id] {
			return false
		}
	}
	return true
}

// SettleLine is the harvest trigger a coordinator acts on.
//
// It carries the fleet-wide attention count and an explicit harvest-everything
// directive, not just the settled pane. Naming only the settled pane trained
// the coordinator into narrow harvests and left sibling work unmerged
// (chainseer, 2026-07-21).
func SettleLine(e Event, attentionCount int) string {
	var b strings.Builder
	b.WriteString("SETTLED ")
	b.WriteString(e.PaneID)
	b.WriteString("=")
	b.WriteString(e.Status)
	if e.Name != "" {
		b.WriteString(" name=")
		b.WriteString(e.Name)
	}
	b.WriteString(" attention=")
	b.WriteString(itoa(attentionCount))
	b.WriteString(" -> HARVEST ALL settled work now, not only this pane")
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
