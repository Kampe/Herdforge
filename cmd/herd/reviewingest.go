package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvestmerge"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// runReviewIngest ports bin/herd-review-ingest: validate reviewer verdict
// artifacts and admit only the well-formed ones to the ledger.
//
// A bad verdict is indistinguishable from a good one once it lands, so every
// refusal happens before the write.
func runReviewIngest() {
	fs := flag.NewFlagSet("review-ingest", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "Validate without writing to the ledger")
	ledgerPath := fs.String("ledger", filepath.Join(".herd", "review-ledger.jsonl"), "Ledger path")
	fs.Parse(os.Args[2:])

	files := fs.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: herd review-ingest <verdict-artifact>... [--dry-run]")
		os.Exit(2)
	}

	coordinators := map[string]struct{}{}
	for k := range reviewledger.DefaultCoordinators {
		coordinators[k] = struct{}{}
	}
	coordinators["herdforge-orchestrator"] = struct{}{}

	commitExists := func(sha string) bool {
		return exec.Command("git", "rev-parse", "--verify", "-q", sha+"^{commit}").Run() == nil
	}

	var ledger *reviewledger.Ledger
	if !*dryRun {
		var err error
		ledger, err = reviewledger.NewReviewLedger(".", *ledgerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-ingest: open ledger: %v\n", err)
			os.Exit(1)
		}
	}

	admitted, refused := 0, 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "REFUSED %s: %v\n", filepath.Base(f), err)
			refused++
			continue
		}
		a := reviewingest.Parse(string(body))
		if err := a.Validate(coordinators, commitExists); err != nil {
			fmt.Fprintf(os.Stderr, "REFUSED %s: %v\n", filepath.Base(f), err)
			refused++
			continue
		}
		if *dryRun {
			fmt.Printf("WOULD_ADMIT %s verdict=%s reviewer=%s sha=%s\n",
				filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12])
			admitted++
			continue
		}
		enqueued, err := ledger.Verdict(reviewledger.VerdictOpts{
			SHA:            a.SHA,
			Reviewer:       a.Reviewer,
			Verdict:        reviewledger.Verdict(a.Verdict),
			Artifact:       f,
			ReviewerFamily: a.ReviewerFamily,
			BuilderFamily:  a.BuilderFamily,
			Branch:         a.Branch,
			CandidateSHA:   a.SHA,
		})
		if err != nil {
			// A ledger write that fails must not be reported as admitted:
			// the verdict does not exist until it is durable.
			fmt.Fprintf(os.Stderr, "herd review-ingest: ledger write FAILED for %s: %v\n", filepath.Base(f), err)
			os.Exit(1)
		}
		fmt.Printf("ADMITTED %s verdict=%s reviewer=%s sha=%s enqueued=%v\n",
			filepath.Base(f), a.Verdict, a.Reviewer, a.SHA[:12], enqueued)
		admitted++
	}

	fmt.Printf("herd review-ingest: admitted=%d refused=%d\n", admitted, refused)
	// Refusals are the signal, not a crash — but a run that admitted nothing
	// while refusing something exits non-zero so a caller cannot mistake it
	// for a clean ingest.
	if admitted == 0 && refused > 0 {
		os.Exit(1)
	}
}

// runHarvestMerge ports bin/herd-harvest-merge: cherry-pick a lane's unique
// commits onto a FRESH worktree off origin/main, gate them, and leave a
// publishable branch.
//
// Picking onto current main rather than pinning at the lane's older HEAD is
// what makes this safe: conflicts surface locally and the merge-base stays at
// origin/main, so the downstream stale-base gate cannot trip on current work.
func runHarvestMerge() {
	fs := flag.NewFlagSet("harvest-merge", flag.ExitOnError)
	branch := fs.String("branch", "", "Lane branch to harvest (required)")
	title := fs.String("title", "", "PR title (required)")
	verdict := fs.String("verdict", "", "Review verdict: PASS merges, FAIL/BLOCKED refuse")
	base := fs.String("base", "origin/main", "Base to harvest onto")
	dryRun := fs.Bool("dry-run", false, "Plan and gate without creating the worktree")

	// Pull the leading positional out BEFORE flag parsing: Go's flag package
	// stops at the first non-flag argument, so `harvest-merge <lane> --branch x`
	// would silently leave --branch unparsed.
	args := os.Args[2:]
	lane := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		lane, args = args[0], args[1:]
	}
	fs.Parse(args)

	if lane == "" || *branch == "" {
		fmt.Fprintln(os.Stderr, "usage: herd harvest-merge <lane> --branch <branch> --title <t> [--verdict PASS]")
		os.Exit(2)
	}

	cherry, err := exec.Command("git", "cherry", *base, *branch).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: git cherry %s %s: %v\n", *base, *branch, err)
		os.Exit(1)
	}
	commits := harvestmerge.UniqueCommits(string(cherry))

	head, _ := exec.Command("git", "rev-parse", *branch).Output()
	plan := harvestmerge.Plan{
		Lane: lane, Branch: *branch, Title: *title,
		SHA:     strings.TrimSpace(string(head)),
		Verdict: harvestmerge.Verdict(strings.ToUpper(*verdict)),
		Commits: commits,
	}
	plan.TempBranch = harvestmerge.TempBranchName(lane, plan.SHA)

	if err := plan.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd harvest-merge: %s has %d unique commit(s) onto %s\n", *branch, len(commits), *base)
	if *dryRun {
		fmt.Printf("herd harvest-merge: DRY_RUN temp_branch=%s\n", plan.TempBranch)
		return
	}

	dir := filepath.Join(".herd", "worktrees", "harvest-"+filepath.Base(plan.TempBranch))
	// Always remove the merge worktree, on every exit path. A leaked worktree
	// is what turned a failed harvest into a blocked retry in chainseer.
	defer func() {
		exec.Command("git", "worktree", "remove", "--force", dir).Run()
		exec.Command("git", "branch", "-D", plan.TempBranch).Run()
	}()

	if out, err := exec.Command("git", "worktree", "add", "-B", plan.TempBranch, dir, *base).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: worktree add: %v: %s\n", err, out)
		os.Exit(1)
	}
	for _, c := range commits {
		if out, err := exec.Command("git", "-C", dir, "cherry-pick", c).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "herd harvest-merge: cherry-pick %s conflicted, aborting before any PR:\n%s\n", c[:min(9, len(c))], out)
			exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
			os.Exit(1)
		}
	}

	// Hard stage gate: a marker in the harvested ADDED lines means the pick
	// produced a structurally broken diff, and once it is in a PR body nobody
	// re-reads it.
	staged, _ := exec.Command("git", "-C", dir, "diff", *base+"...HEAD").Output()
	if markers := harvestmerge.ConflictMarkers(string(staged)); len(markers) > 0 {
		fmt.Fprintf(os.Stderr, "herd harvest-merge: REFUSED — %d conflict marker(s) in the harvested diff:\n", len(markers))
		for _, m := range markers {
			fmt.Fprintf(os.Stderr, "  %s\n", m)
		}
		os.Exit(1)
	}

	fmt.Printf("herd harvest-merge: harvested %d commit(s) clean onto %s at %s\n", len(commits), *base, dir)
	fmt.Println("herd harvest-merge: gates passed. Publish and merge is the coordinator's explicit action:")
	fmt.Printf("  git push -u origin %s && gh pr create --title %q\n", plan.TempBranch, *title)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
