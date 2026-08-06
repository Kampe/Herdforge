// Package feedback ports bin/herd-feedback: a periodic fleet-wide
// control-plane feedback census.
//
// The coordinator only ever sees what it thought to ask about. This asks every
// live lane the questions the coordinator cannot ask itself — what is blocked,
// what capacity is idle, which prompt never landed, where quota disagrees with
// pane status, and what the coordinator missed entirely.
//
// Durable inbox delivery is authoritative; a settled agent additionally gets a
// wake nudge, but a failed nudge is a warning, never a lost request.
package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultInterval is how often a new census opens.
	DefaultInterval = 30 * time.Minute
	// DefaultGrace is how long a lane has to reply before it is reported missing.
	DefaultGrace = 10 * time.Minute
	// SubjectPrefix marks a census request and its replies.
	SubjectPrefix = "FLEET_FEEDBACK"
)

// State is the durable census record.
type State struct {
	Epoch            string   `json:"epoch"`
	RequestedAtEpoch int64    `json:"requested_at_epoch"`
	Lanes            []string `json:"lanes"`
}

// Census is one evaluation of the feedback loop.
type Census struct {
	Epoch    string
	Lanes    []string
	Replied  []string
	Missing  []string
	Overdue  bool
	Started  bool
	Skipped  string
	Interval time.Duration
	Grace    time.Duration
}

// StatePath is the durable census file.
func StatePath(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = filepath.Join(".herd", "feedback")
	}
	return filepath.Join(stateDir, "current.json")
}

// Load reads the durable census, treating absence as "no census yet". A
// corrupt file is an error, not an implicit fresh start: silently discarding
// it would drop the outstanding request set and report a false all-clear.
func Load(stateDir string) (*State, error) {
	path := StatePath(stateDir)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("feedback: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &State{}, nil
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("feedback: corrupt census %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the census atomically so a crash mid-write cannot leave a
// half-parsed outstanding set behind.
func Save(stateDir string, s *State) error {
	path := StatePath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("feedback: create state dir: %w", err)
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("feedback: marshal census: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("feedback: write census: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("feedback: commit census: %w", err)
	}
	return nil
}

// Epoch stamps a census. Callers pass the clock so this stays testable.
func Epoch(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// Missing returns requested lanes that have not replied, deterministically
// sorted so two runs of the same state produce identical output.
func Missing(requested, replied []string) []string {
	got := make(map[string]bool, len(replied))
	for _, r := range replied {
		got[r] = true
	}
	var missing []string
	for _, want := range requested {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	return missing
}

// Due reports whether a new census should open.
func Due(last time.Time, now time.Time, interval time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}

// Overdue reports whether outstanding replies have blown the grace window.
func Overdue(requestedAt time.Time, now time.Time, grace time.Duration, missing int) bool {
	if missing == 0 || requestedAt.IsZero() {
		return false
	}
	return now.Sub(requestedAt) >= grace
}

// RequestBody is the exact census prompt. It names the reply command so a lane
// cannot answer in a shape the census is unable to count, and it demands an
// explicit NONE per field so silence is never mistaken for "nothing to report".
func RequestBody(epoch, coordinator string) string {
	return fmt.Sprintf(
		"FLEET FEEDBACK REQUEST %s. Before your next handoff, inspect beyond your assigned task and report: "+
			"blocker or underutilized capacity; any prompt that was not consumed; quota/provider state that "+
			"disagrees with pane status; and anything the coordinator or herd tooling missed. "+
			"Reply exactly with: herd mail send %s --summary '%s %s <your-lane>' "+
			"'blocker=<...>; delivery=<...>; quota=<...>; coordinator_blind_spot=<...>' "+
			"Use NONE explicitly for each empty field. Do not mutate outside your assigned worktree.",
		epoch, coordinator, SubjectPrefix, epoch)
}

// Subject is the census subject for one epoch.
func Subject(epoch string) string { return SubjectPrefix + " " + epoch }

// NeedsWake reports whether a settled lane should also get a nudge. A working
// or starting agent is left alone: the durable inbox copy already exists and
// interrupting active work to deliver a census is worse than waiting.
func NeedsWake(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "idle", "done", "blocked", "unknown", "":
		return true
	}
	return false
}
