package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// DrainPartial is what a bounded drain can attest when it ran out of budget.
//
// FAC-555: the command previously exited with only "bounded scan exceeded
// 2m0s: context deadline exceeded" -- no counts, no candidate identities, no
// remaining figure. That is unusable on a mature board: an operator cannot
// tell whether there is nothing to drain or whether the scan never got there.
// Partial evidence beats a bare deadline, provided it is clearly labelled
// partial and never mistaken for a complete report.
type DrainPartial struct {
	Partial bool     `json:"partial"`
	Phase   string   `json:"failed_phase"`
	Scanned int      `json:"harvest_scanned"`
	Errors  []string `json:"harvest_input_errors,omitempty"`
	// Candidates are the branch identities harvest-scan resolved before the
	// bound expired. These are candidates to inspect, never proof of anything.
	Candidates []string `json:"candidates,omitempty"`
	Note       string   `json:"note"`
}

const drainPartialNote = "PARTIAL result: the review-scan phase did not complete, so queue pressure, " +
	"cap posture, and dispositions are unknown. Counts below cover harvest-scan only and must not " +
	"be read as a drain decision."

// emitDrainPartial writes the harvest-scan partial. It is deliberately explicit
// that the result is incomplete: a partial silently shaped like a full report is
// worse than the bare timeout it replaces.
func emitDrainPartial(out, errOut io.Writer, result *harvest.HarvestResult, asJSON bool) {
	if result == nil {
		fmt.Fprintln(errOut, "herd-drain: partial=none — harvest-scan produced no result")
		return
	}
	branches := make([]string, 0, len(result.UnmergedWorktrees))
	for _, w := range result.UnmergedWorktrees {
		if w.Branch != "" {
			branches = append(branches, w.Branch)
		}
	}
	sort.Strings(branches)

	partial := DrainPartial{
		Partial:    true,
		Phase:      "review-scan",
		Scanned:    len(result.UnmergedWorktrees),
		Errors:     result.Errors,
		Candidates: branches,
		Note:       drainPartialNote,
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(partial); err != nil {
			fmt.Fprintf(errOut, "herd-drain: encode partial: %v\n", err)
		}
		return
	}
	fmt.Fprintf(errOut, "herd-drain: PARTIAL failed_phase=review-scan harvest_scanned=%d input_errors=%d\n",
		partial.Scanned, len(partial.Errors))
	for _, b := range branches {
		fmt.Fprintf(errOut, "  candidate branch: %s\n", b)
	}
	fmt.Fprintf(errOut, "  %s\n", drainPartialNote)
}
