package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/beat"
)

// runUtilization publishes the per-lane utilization beat.
//
// FAC-706: fleet health was being read off resident tab counts. Eleven lanes
// sat present and idle while every pane-counting summary reported a healthy
// fleet, and the operator had to notice by hand that nothing was moving.
//
// This reports the one condition that matters -- idle, unheld, with work
// available -- and NAMES it a failed beat rather than folding it into a total.
//
// It projects from attention's own triage rather than recomputing lane status
// and hold state. Two places deciding whether a lane is held is exactly the
// duplication that produced FAC-660/694/695/699/701, where the same lane
// matching bug had to be fixed in four files. One source, one answer.
func runUtilization(args []string, result *attention.Result) error {
	fs := flag.NewFlagSet("utilization", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the structured utilization beat")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if result == nil {
		// An unreadable triage is UNKNOWN, never an empty fleet. Reporting zero
		// lanes here would be the exact defect this command exists to remove.
		return fmt.Errorf("attention triage unavailable, so utilization is UNKNOWN rather than empty")
	}

	// A wake queue nobody reads is the same dead handoff it exists to prevent,
	// so the utilization beat is its consumer. Problems are REPORTED rather
	// than fatal: an unreadable wake must not hide the lane states underneath.
	wakes, wakeProblems := beat.PendingWakes(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "."))
	for _, p := range wakeProblems {
		fmt.Fprintf(os.Stderr, "herd-utilization: %v\n", p)
	}

	states := make([]beat.LaneState, 0, len(result.Items))
	for _, it := range result.Items {
		st := beat.LaneState{
			Lane:    it.Name,
			Status:  it.Status,
			Blocker: it.Reason,
		}
		// attention marks a parked lane by carrying its hold reason. A held
		// lane is doing exactly what an operator asked, so it can never be a
		// failed beat.
		st.Held = strings.Contains(strings.ToLower(it.Reason), "held")

		// "Work available" is deliberately conservative: a lane attention
		// flagged as needing eyes has something to answer for. A lane it did
		// not flag is resting, and counting that as a failure would make the
		// number meaningless.
		st.WorkAvailable = attention.NeedsEyes(it.Level) && !st.Held

		// FAC-707: a wake RECORDED at the event that produced it beats a wake
		// guessed here. `herd kick` is a generic nudge; the enqueued action is
		// the exact next thing this lane was told to do when its last handoff
		// completed. Prefer the recorded one, and say what caused it.
		if w, ok := wakes[it.Name]; ok {
			st.NextWake = w.Action
			if w.Cause != "" {
				st.NextWake += "   (from " + string(w.Event) + ": " + w.Cause + ")"
			}
		} else if it.Status == "missing" {
			st.NextWake = "herd standing --only " + it.Name
		} else {
			st.NextWake = "herd kick"
		}
		states = append(states, st)
	}

	u := beat.ProjectUtilization(states)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(u)
	}
	fmt.Println(u.Summarize())
	for _, l := range u.Lanes {
		if !l.FailedBeat {
			continue
		}
		fmt.Printf("  FAILED BEAT %s (%s) — %s → %s\n", l.Lane, l.Status, l.Blocker, l.NextWake)
	}
	return nil
}
