package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/readyindex"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// loadExactReadyTips is the FAC-603 default discovery path: ready candidates
// come from the exact-ready index (projection of the harvest queue), not from
// a full worktree tip scan. Cost is bounded by the ready-set size.
func loadExactReadyTips(root, ledgerPath string, errOut io.Writer, quiet bool) ([]harvest.UnmergedWork, int, error) {
	if !quiet {
		fmt.Fprintln(errOut, "herd-drain: phase=exact-ready-index")
	}
	start := time.Now()
	indexPath := readyindex.PathFor(ledgerPath)
	entries, err := readyindex.List(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, 0, fmt.Errorf("exact-ready index: %w", err)
	}
	if os.IsNotExist(err) || len(entries) == 0 {
		// Projection missing or empty: rebuild from authoritative Queued().
		l, lerr := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
		if lerr != nil {
			return nil, 0, fmt.Errorf("exact-ready ledger: %w", lerr)
		}
		queued, qerr := l.Queued()
		if qerr != nil {
			return nil, 0, fmt.Errorf("exact-ready queued: %w", qerr)
		}
		entries = make([]readyindex.Entry, 0, len(queued))
		for _, row := range queued {
			entries = append(entries, readyindex.Entry{
				SHA: row.SHA, Branch: row.Branch, Lane: row.Lane, Reviewer: row.Reviewer, Updated: row.Timestamp,
			})
		}
		if rerr := readyindex.Rebuild(indexPath, entries, "rebuild"); rerr != nil {
			fmt.Fprintf(errOut, "herd-drain: exact-ready projection rebuild: %v\n", rerr)
		}
	}
	tips := make([]harvest.UnmergedWork, 0, len(entries))
	for _, e := range entries {
		if e.SHA == "" {
			continue
		}
		tips = append(tips, harvest.UnmergedWork{
			Branch:   e.Branch,
			Unmerged: []string{e.SHA},
		})
	}
	if !quiet {
		fmt.Fprintf(errOut, "herd-drain: phase=exact-ready-index done in %s: ready_candidates=%d (bounded independent of worktree count)\n",
			time.Since(start).Round(time.Millisecond), len(tips))
	}
	return tips, len(tips), nil
}

// rebuildExactReadyFromQueued refreshes the projection after an explicit --repair
// full scan so the next default drain stays indexed.
func rebuildExactReadyFromQueued(root, ledgerPath string, errOut io.Writer) {
	l, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		fmt.Fprintf(errOut, "herd-drain: exact-ready repair rebuild: %v\n", err)
		return
	}
	queued, err := l.Queued()
	if err != nil {
		fmt.Fprintf(errOut, "herd-drain: exact-ready repair queued: %v\n", err)
		return
	}
	entries := make([]readyindex.Entry, 0, len(queued))
	for _, row := range queued {
		entries = append(entries, readyindex.Entry{
			SHA: row.SHA, Branch: row.Branch, Lane: row.Lane, Reviewer: row.Reviewer, Updated: row.Timestamp,
		})
	}
	if err := readyindex.Rebuild(readyindex.PathFor(ledgerPath), entries, "repair"); err != nil {
		fmt.Fprintf(errOut, "herd-drain: exact-ready repair write: %v\n", err)
		return
	}
	fmt.Fprintf(errOut, "herd-drain: exact-ready index rebuilt: entries=%d\n", len(entries))
}
