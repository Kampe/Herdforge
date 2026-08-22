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

// QueueLeaf is the harvest queue's filename, which lives beside the ledger.
const QueueLeaf = "harvest-queue.jsonl"

// QueuePathFor derives the harvest queue path from a ledger path.
//
// FAC-575: NewReviewLedger derived this inline and a second constructor
// repeated it. The queue is only meaningful relative to its ledger, so deriving
// it in two places is how a caller ends up reading a ledger and a queue that do
// not belong to each other.
func QueuePathFor(ledgerPath string) string {
	return filepath.Join(filepath.Dir(ledgerPath), QueueLeaf)
}
