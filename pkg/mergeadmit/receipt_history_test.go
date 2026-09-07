package mergeadmit

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

func TestReconcileFollowupRequiresFreshEvidenceAndRetainsPrior(t *testing.T) {
	for _, mode := range []string{"reduced", "full"} {
		t.Run(mode, func(t *testing.T) {
			dir := gitRepo(t)
			base := commit(t, dir, "base.txt", "base\n", "base")
			first := commit(t, dir, "first.txt", "first\n", "first")
			ledger := newLedger(t, dir)
			launch(t, ledger, first, "reviewer-a", "anthropic", "builder-session-1")
			verdict(t, ledger, first, "reviewer-a", reviewledger.VerdictPASS)
			gate := &Gate{RepoDir: dir, Ledger: ledger, Policy: testPolicy(), Live: LiveState{OriginMain: StaticProbe(first)}}
			request := okRequest(base, first)
			if mode == "reduced" {
				request.ReducedProvenance = &ReducedProvenance{PullRequest: 1, VerifyLanded: true}
			}
			old, err := gate.ReconcileLanded(request)
			if err != nil {
				t.Fatal(err)
			}
			path := hsync.ReceiptPath(dir, testRef)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			next := commit(t, dir, "second.txt", "second\n", "follow-up")
			gate.Live.OriginMain = StaticProbe(next)
			request = okRequest(first, next)
			if mode == "reduced" {
				request.ReducedProvenance = &ReducedProvenance{PullRequest: 2, VerifyLanded: true}
			}
			request.PriorReceiptDigest = old.Digest
			if _, err := gate.ReconcileLanded(request); err == nil {
				t.Fatal("prior receipt substituted for new review")
			}
			launch(t, ledger, next, "reviewer-b", "anthropic", "builder-session-1")
			verdict(t, ledger, next, "reviewer-b", reviewledger.VerdictPASS)
			bad := request
			bad.PriorReceiptDigest = strings.Repeat("a", 64)
			if _, err := gate.ReconcileLanded(bad); err == nil {
				t.Fatal("wrong predecessor accepted")
			}
			bad = request
			bad.BaseSHA = base
			if _, err := gate.ReconcileLanded(bad); err == nil {
				t.Fatal("discontinuous reviewed base accepted")
			}
			bad = request
			bad.PriorReceiptDigest = ""
			if _, err := gate.ReconcileLanded(bad); err == nil {
				t.Fatal("default reconcile replaced existing receipt")
			}
			unchanged, _ := os.ReadFile(path)
			if !bytes.Equal(before, unchanged) {
				t.Fatal("refusal changed receipt")
			}
			current, err := gate.ReconcileLanded(request)
			if err != nil {
				t.Fatal(err)
			}
			if current.CandidateSHA != next || current.Digest == old.Digest {
				t.Fatal("follow-up identity lost")
			}
			retained, err := hsync.LoadPriorReceipt(dir, testRef, old.Digest)
			if err != nil || retained.Digest != old.Digest {
				t.Fatalf("history: %v", err)
			}
			if _, err := gate.ReconcileLanded(request); err != nil {
				t.Fatalf("idempotent replay: %v", err)
			}
			launch(t, ledger, next, "reviewer-c", "anthropic", "builder-session-1")
			verdict(t, ledger, next, "reviewer-c", reviewledger.VerdictFAIL)
			if _, err := gate.ReconcileLanded(request); err == nil {
				t.Fatal("consumed replay ignored current dissent")
			}
		})
	}
}
