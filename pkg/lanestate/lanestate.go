// Package lanestate ports bin/herd-seed-lane-state: create a standing lane's
// state artifacts if absent, preferring a durable snapshot over a blank
// template.
//
// These files are gitignored and lane-local, so they cannot survive a
// wind-down by construction: every standing worktree is removed and recreated,
// and `git worktree add` makes an empty directory. chainseer lost all twelve
// lanes' clock-outs and recorded decisions this way on 2026-07-29. A rebase
// across the commit that untracked them does the same thing mid-session, with
// no reseed trigger — one lane only noticed when an edit errored file-not-found.
//
// Restoring is never worse than the blank template it replaces, because only
// verified-good state is ever snapshotted.
//
// It deliberately never writes a progress session entry. A fabricated clock-out
// would defeat the ledger; a freshly seeded file is SUPPOSED to read as
// incomplete until the lane clocks out for real.
package lanestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Artifact names a lane-local state file.
type Artifact string

const (
	WorkState    Artifact = "WORK-STATE.json"
	LaneProgress Artifact = "LANE-PROGRESS.md"
)

// Outcome is what happened to one artifact.
type Outcome string

const (
	// Restored: recovered from the durable snapshot. Continuity preserved.
	Restored Outcome = "RESTORED"
	// Seeded: written blank from a template. A genuine first run — or lost
	// continuity, which is what ContinuityLost detects.
	Seeded Outcome = "SEEDED"
	// Present: already there, left untouched.
	Present Outcome = "PRESENT"
	// Failed: could not restore or seed.
	Failed Outcome = "FAILED"
)

// Result records one artifact's fate.
type Result struct {
	Artifact Artifact
	Outcome  Outcome
	Detail   string
}

// SnapshotDir is where verified-good lane state is kept, outside any worktree.
func SnapshotDir(stateRoot, lane string) string {
	return filepath.Join(stateRoot, "lane-state", lane)
}

// MailPath is the lane's durable inbox. Its existence is independent evidence
// the lane has run before, because it lives outside the worktree and survives
// every wind-down.
func MailPath(stateRoot, lane string) string {
	return filepath.Join(stateRoot, "mail", lane+".jsonl")
}

// ContinuityLost reports the case that must never be silent: a BLANK ledger was
// seeded for a lane that demonstrably ran before.
//
// Reaching here also indicts the snapshot mechanism itself — if restore had
// worked there would have been nothing to seed — so it means the snapshot is
// missing or unreadable, not merely that this is a fresh lane.
func ContinuityLost(results []Result, hasPriorMail bool) bool {
	if !hasPriorMail {
		return false
	}
	seeded, restored := false, false
	for _, r := range results {
		switch r.Outcome {
		case Seeded:
			seeded = true
		case Restored:
			restored = true
		}
	}
	return seeded && !restored
}

// blankWorkState is the template ledger. No features, no clock-out.
func blankWorkState(lane string, now time.Time) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"lane":         lane,
		"last_updated": now.Format("2006-01-02"),
		"rules": map[string]bool{
			"single_active_feature":                       true,
			"passing_requires_evidence":                   true,
			"states_change_only_after_running_verification": true,
		},
		"features": []any{},
	}, "", "  ")
}

const blankLaneProgress = `# Lane Progress

<!-- Seeded blank. This file stays incomplete until the lane clocks out for
     real; a fabricated session entry would defeat the ledger. -->

## Sessions
`

// Seed restores or creates a lane's state artifacts in worktree wt.
// Existing files are never touched, so a lane's real ledger is never
// overwritten. Callers exit 0 regardless: a missing ledger must never keep a
// lane down.
func Seed(wt, lane, stateRoot string, now time.Time) []Result {
	src := SnapshotDir(stateRoot, lane)
	results := make([]Result, 0, 2)

	for _, a := range []Artifact{WorkState, LaneProgress} {
		dst := filepath.Join(wt, string(a))
		if _, err := os.Stat(dst); err == nil {
			results = append(results, Result{Artifact: a, Outcome: Present})
			continue
		}
		// Prefer the durable snapshot: only verified-good state is snapshotted,
		// so a restore can never be worse than the blank template.
		if body, err := os.ReadFile(filepath.Join(src, string(a))); err == nil {
			if err := os.WriteFile(dst, body, 0o644); err == nil {
				results = append(results, Result{Artifact: a, Outcome: Restored,
					Detail: "continuity recovered from " + src + ", NOT a first run"})
				continue
			}
		}
		var body []byte
		var err error
		if a == WorkState {
			body, err = blankWorkState(lane, now)
		} else {
			body = []byte(blankLaneProgress)
		}
		if err != nil {
			results = append(results, Result{Artifact: a, Outcome: Failed, Detail: err.Error()})
			continue
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			results = append(results, Result{Artifact: a, Outcome: Failed, Detail: err.Error()})
			continue
		}
		results = append(results, Result{Artifact: a, Outcome: Seeded, Detail: "lane-local"})
	}
	return results
}

// ContinuityWarning is the exact text for a lost-continuity lane.
func ContinuityWarning(lane, src string) string {
	return fmt.Sprintf("WARN %s seeded a BLANK ledger but has prior mail traffic, so this is NOT a first run: "+
		"continuity was LOST and no snapshot was available at %s. "+
		"Re-derive state from git log and the lane inbox before assuming a clean slate.", lane, src)
}
