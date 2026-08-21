package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/candidate"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// liveGit is the read-only git surface for candidate resolution.
//
// FAC-568: every branch answer here comes from git for the exact object. A live
// case had an admitted artifact naming standing/platform-ops while the SHA was
// only reachable from standing/coverage-integrity, so a recorded branch is a
// hint to report, never the thing to harvest.
type liveGit struct{ repoRoot string }

func (g liveGit) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", g.repoRoot}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func (g liveGit) ObjectExists(_ context.Context, sha string) bool {
	_, err := g.git("cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (g liveGit) BranchesContaining(_ context.Context, sha string) ([]string, error) {
	out, err := g.git("for-each-ref", "--format=%(refname:short)", "--contains", sha,
		"refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		if b := strings.TrimSpace(line); b != "" {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

func (g liveGit) WorktreeForBranch(_ context.Context, branch string) (string, error) {
	return worktreeForBranch(branch), nil
}

func (g liveGit) ContainedInMain(_ context.Context, sha string) bool {
	_, _ = g.git("fetch", "-q", "origin", "main")
	if _, err := g.git("rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return false
	}
	_, err := g.git("merge-base", "--is-ancestor", sha, "origin/main")
	return err == nil
}

func (g liveGit) PatchLandedOnMain(ctx context.Context, sha string) bool {
	merged, err := harvest.ContentMerged(ctx, g.repoRoot, "origin/main", sha)
	return err == nil && merged
}

// ledgerReviews resolves admitted review evidence per ref from the ONE
// canonical review ledger (FAC-565).
type ledgerReviews struct{ path string }

func (l ledgerReviews) AdmittedForRef(ref string) ([]candidate.Review, error) {
	snap, err := review.OpenLedger(l.path).Snapshot()
	if err != nil {
		return nil, fmt.Errorf("read review ledger: %w", err)
	}
	withdrawn := map[string]bool{}
	for _, row := range snap.Rows {
		if row.Event == string(reviewledger.EventRevoked) || row.Event == string(reviewledger.EventSupersession) {
			withdrawn[row.SHA] = true
		}
	}
	merged := map[string]candidate.Review{}
	for _, row := range snap.Rows {
		if row.SHA == "" || withdrawn[row.SHA] || row.Verdict == "" {
			continue
		}
		if !rowNamesRef(row, ref) {
			continue
		}
		cur := merged[row.SHA]
		cur.CandidateSHA = row.SHA
		// Later rows enrich earlier ones; never overwrite a known value with a
		// blank, or a follow-up row erases the evidence.
		cur.Verdict = firstNonEmpty(row.Verdict, cur.Verdict)
		cur.RecordedBranch = firstNonEmpty(row.Branch, cur.RecordedBranch)
		cur.Artifact = firstNonEmpty(row.Artifact, cur.Artifact)
		cur.Reviewer = firstNonEmpty(row.Reviewer, cur.Reviewer)
		cur.ReviewerFamily = firstNonEmpty(row.ReviewerFamily, cur.ReviewerFamily)
		cur.BuilderFamily = firstNonEmpty(row.BuilderFamily, cur.BuilderFamily)
		cur.MergeSHA = firstNonEmpty(row.MergeSHA, cur.MergeSHA)
		merged[row.SHA] = cur
	}
	out := make([]candidate.Review, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// runCandidate reports the exact candidate identity for a ref: SHA, the
// branches that ACTUALLY contain it, worktree, families, verdict, landing, and
// one disposition. Read-only.
func runCandidate(args []string) {
	asJSON := false
	var refs []string
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "herd candidate: unknown flag %q\n", a)
			os.Exit(2)
		default:
			refs = append(refs, a)
		}
	}
	if len(refs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: herd candidate <ref>... [--json]")
		os.Exit(2)
	}

	reviews := ledgerReviews{path: reviewLedgerPath()}
	git := liveGit{repoRoot: "."}
	out := make([]*candidate.Identity, 0, len(refs))
	failed := false
	for _, ref := range refs {
		id, err := candidate.Resolve(context.Background(), ref, reviews, git)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd candidate: %v\n", err)
			failed = true
			continue
		}
		out = append(out, id)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "herd candidate: encode: %v\n", err)
			os.Exit(1)
		}
	} else {
		for _, id := range out {
			fmt.Printf("%s %s\n  disposition: %s — %s\n", id.Ref, shortSHA12(id.CandidateSHA), id.Disposition, id.Detail)
			if len(id.Branches) > 0 {
				fmt.Printf("  branches (from git): %s\n", strings.Join(id.Branches, ", "))
			}
			if id.BranchMismatch {
				fmt.Printf("  WARNING recorded branch %q does not contain this object — harvest by SHA\n", id.RecordedBranch)
			}
			if id.Worktree != "" {
				fmt.Printf("  worktree: %s\n", id.Worktree)
			}
			if id.Verdict != "" {
				fmt.Printf("  verdict: %s reviewer=%s (%s) builder=%s\n",
					id.Verdict, id.Reviewer, id.ReviewerFamily, id.BuilderFamily)
			}
			fmt.Printf("  landing: %s\n", id.Landing)
			for _, alt := range id.Alternatives {
				fmt.Printf("  alternative candidate: %s\n", alt)
			}
		}
	}
	if failed {
		os.Exit(1)
	}
}
