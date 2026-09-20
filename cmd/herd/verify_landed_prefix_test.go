package main

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-836: an early complete match must leave enough of the SAME allowance
// for sealing. This drives the public entry, including both origin reads and
// proofs, then asks the shipped receipt consumer to accept the result.
func TestRunHarvestVerifyLandedStopsInspectingAfterOrderedMatch(t *testing.T) {
	for _, later := range []int{0, 48} {
		t.Run(fmt.Sprintf("later-%d", later), func(t *testing.T) {
			repo, candidate, binding := publicEntryFixture(t)
			// Reuse authentic review evidence, but publish the original two-commit
			// stack rather than the fixture's squash. Only this test's disposable
			// clone and bare origin are rewritten. The carrier stays retired.
			surfaceGit(t, repo, "reset", "--hard", candidate)
			for i := 0; i < later; i++ {
				surfaceCommit(t, repo, "unrelated", fmt.Sprintf("later main change %d\n", i))
			}
			surfaceGit(t, repo, "push", "--force", "-q", "origin", "main")
			if got := surfaceGit(t, repo, "rev-list", "--count", binding.BaseSHA+"..origin/main"); got != strconv.Itoa(later+2) {
				t.Fatalf("fixture does not contain the two reviewed commits plus %d later commits: %s", later, got)
			}
			if got := surfaceGit(t, repo, "rev-list", "--count", binding.BaseSHA+".."+candidate); got != "2" {
				t.Fatalf("fixture is not a complete two-commit reviewed stack: %s", got)
			}

			// Fixed independently of the implementation: 64 covers the native
			// short proof AND seal. Inspecting all 50 landed diffs first cannot
			// fit the advanced case. No measured threshold can follow the mutant.
			previous := cliProofBudget
			cliProofBudget = mergeadmit.ProofBudget{MaxCommands: 64}
			t.Cleanup(func() { cliProofBudget = previous })
			if err := runHarvestVerifyLanded(pinProofBranch, binding); err != nil {
				if errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
					t.Fatalf("complete ordered landing spent its allowance inspecting later history: %v", err)
				}
				t.Fatalf("ordered landing failed outside the command-budget oracle: %v", err)
			}
			assertReceiptAndDispositionAgree(t, repo)
			receipt, err := hsync.LoadReceipt(hsync.ReceiptPath(repo, pinProofRef))
			if err != nil {
				t.Fatal(err)
			}
			if receipt.CandidateSHA != candidate || receipt.BaseSHA != binding.BaseSHA || receipt.MergeSHA != candidate {
				t.Fatalf("ordered proof changed the reviewed range or its first complete match: %+v", receipt)
			}
			if err := receipt.Validate(repo, pinProofRef, nil); err != nil {
				t.Fatalf("public consumer refused the early ordered landing receipt: %v", err)
			}
		})
	}
}
