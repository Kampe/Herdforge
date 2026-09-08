package mergeadmit

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func hostIngestOn(t *testing.T, l *reviewledger.Ledger, sha, host, digest string, v reviewledger.Verdict, family string) {
	t.Helper()
	commitTime := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	_, err := l.HostIngest(reviewledger.HostIngestOpts{
		SHA: sha, Reviewer: "reviewer-a", Task: testRef, Branch: "work",
		Artifact: host + ".md", ArtifactDigest: digest, Verdict: v,
		ReviewerFamily: family, BuilderFamily: reviewledger.FamilyUnrecorded, VfyDigest: host + "-vfy",
		ReadBase: "prod-parent", ReadHead: sha, ProductionBase: "prod-parent",
		CommitTime: commitTime,
		Reaches:    func(branch, got string) bool { return branch == "work" && got == sha },
		Receipt: reviewledger.LaunchProvenance{
			Host: host, Session: host, BuilderFamily: "openai", Branch: "work",
			CreatedAt: time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC),
			Accepted:  true, Member: true,
		},
	})
	if err != nil {
		t.Fatalf("HostIngest host=%s verdict=%s: %v", host, v, err)
	}
}

func TestReconcileLandedReducedRefusesAuthenticatedCrossHostDissent(t *testing.T) {
	failDig := "fa2624fb531c84a55cbcfbc346b2ff39b75c0c63d510744bd745daaa4af8fd5c1"
	passDig := "e2624fb531c84a55cbcfbc346b2ff39b75c0c63d510744bd745daaa4af8fd5c1"
	orders := [][]struct {
		host, family, digest string
		verdict              reviewledger.Verdict
	}{
		{
			{"host-a", "google", failDig, reviewledger.VerdictFAIL},
			{"host-b", "xai", passDig, reviewledger.VerdictPASS},
		},
		{
			{"host-b", "xai", passDig, reviewledger.VerdictPASS},
			{"host-a", "google", failDig, reviewledger.VerdictFAIL},
		},
		{
			{"host-a", "google", failDig, reviewledger.VerdictBLOCKED},
			{"host-b", "xai", passDig, reviewledger.VerdictPASS},
		},
		{
			{"host-b", "xai", passDig, reviewledger.VerdictPASS},
			{"host-a", "google", failDig, reviewledger.VerdictBLOCKED},
		},
	}
	for _, seq := range orders {
		name := string(seq[0].verdict) + "_then_" + string(seq[1].verdict)
		t.Run(name, func(t *testing.T) {
			dir := gitRepo(t)
			base := commit(t, dir, "a.txt", "one\n", "base")
			run(t, dir, "git", "checkout", "-q", "-b", "work")
			candidate := commit(t, dir, "b.txt", "two\n", "candidate")
			landed := rewriteOnto(t, dir, "landed", base, []string{candidate})
			l := newLedger(t, dir)
			launch(t, l, candidate, "reviewer-a", "anthropic", "builder-session-1")
			verdict(t, l, candidate, "reviewer-a", reviewledger.VerdictPASS)
			for _, step := range seq {
				hostIngestOn(t, l, candidate, step.host, step.digest, step.verdict, step.family)
			}
			g := &Gate{
				RepoDir: dir, Ledger: l, Policy: testPolicy(),
				Live: LiveState{OriginMain: StaticProbe(landed)},
			}
			req := Request{
				Ref: testRef, CandidateSHA: candidate, BaseSHA: base,
				ReducedProvenance: &ReducedProvenance{PullRequest: 2864, VerifyLanded: true},
			}
			var opened int
			for i := 0; i < 32; i++ {
				_, err := g.ReconcileLanded(req)
				if err == nil {
					opened++
					continue
				}
				if !strings.Contains(err.Error(), CodeLedgerRefused) {
					t.Fatalf("iteration %d: want %s, got %v", i, CodeLedgerRefused, err)
				}
			}
			if opened > 0 {
				t.Fatalf("reduced ReconcileLanded fail-opened on %d/32 iterations", opened)
			}
		})
	}
}
