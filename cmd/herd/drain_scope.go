package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/review"
)

// receiptedCandidateSHAs reads every completion receipt and returns the exact
// candidate/merge object names they name.
//
// FAC-559: control-lane branches (standing/*, herd/*) genuinely carry real
// candidate commits, so they must not be prefix-excluded. The consumer's rule is
// that only an EXACT receipted SHA counts -- a moving standing branch tip is
// control-lane state, not one harvestable candidate. That makes the receipt set
// the authority, not the branch name.
func receiptedCandidateSHAs(repoRoot string) map[string]bool {
	out := map[string]bool{}
	dir := filepath.Join(repoRoot, ".herd", "receipts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No receipts readable. Callers treat an empty set as "cannot prove",
		// which keeps receipt-gated branches in scope rather than hiding them.
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		var r struct {
			CandidateSHA string `json:"candidate_sha"`
			MergeSHA     string `json:"merge_sha"`
		}
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		for _, sha := range []string{r.CandidateSHA, r.MergeSHA} {
			if sha = strings.TrimSpace(sha); sha != "" {
				out[sha] = true
			}
		}
	}
	return out
}

// drainReceiptOracle builds the receipt predicate for candidate scoping.
//
// It fails OPEN deliberately: when there are no readable receipts at all,
// nothing can be proven a non-candidate, so receipt-gated branches stay in
// scope. Hiding a real candidate is worse than scanning a scratch branch.
func drainReceiptOracle(repoRoot string) review.HasCandidateReceipt {
	receipted := receiptedCandidateSHAs(repoRoot)
	if len(receipted) == 0 {
		return nil
	}
	return func(w harvest.UnmergedWork) bool {
		for _, sha := range w.Unmerged {
			sha = strings.TrimSpace(sha)
			if sha == "" {
				continue
			}
			if receipted[sha] {
				return true
			}
			// Receipts may record an abbreviated object name.
			for candidate := range receipted {
				if len(candidate) >= 7 && (strings.HasPrefix(sha, candidate) || strings.HasPrefix(candidate, sha)) {
					return true
				}
			}
		}
		return false
	}
}
