package reviewledger

import (
	"path/filepath"
	"strings"
)

// Leaf is the review ledger's filename under .herd.
const Leaf = "review-ledger.jsonl"

// DefaultPath is THE definition of where the review ledger lives.
//
// FAC-573: six callers rebuilt this path from segments, split between a
// cwd-relative form and a root-prefixed one. The review ledger is the record of
// what was reviewed and admitted, and this repository has already produced two
// consumer-visible defects from location divergence — a mailbox and a review
// corpus each resolving differently depending on the caller's working directory.
// A ledger that resolves differently per caller is the same defect with worse
// consequences, because a missing ledger row reads as "never reviewed".
//
// An empty root yields the repo-relative form, so this is a drop-in for both
// call shapes.
func DefaultPath(root string) string {
	if strings.TrimSpace(root) == "" {
		return filepath.Join(".herd", Leaf)
	}
	return filepath.Join(root, ".herd", Leaf)
}
