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
	// ANY refusal is a non-zero exit. Exiting 0 on a partial batch let
	// `herd review-ingest *.md && <next step>` proceed with silently rejected
	// verdicts, which is precisely the fail-open this gate exists to prevent
	// (CLAUDE.md invariant 2).
	if refused > 0 {
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
	allowMarkers := fs.Bool("allow-markers", false,
		"Proceed despite conflict markers in the harvested diff (for files whose CONTENT is marker fixtures)")

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

	// The harvest body is a closure returning an error rather than calling
	// os.Exit inline. os.Exit does NOT run deferred functions, so a defer here
	// plus os.Exit on the failure paths would skip cleanup on exactly the
	// conflict and marker paths where a leaked worktree matters most — the very
	// trap chainseer's EXIT trap exists to prevent, reintroduced.
	err = func() error {
		if out, addErr := exec.Command("git", "worktree", "add", "-B", plan.TempBranch, dir, *base).CombinedOutput(); addErr != nil {
			return fmt.Errorf("worktree add: %v: %s", addErr, out)
		}
		for _, c := range commits {
			out, pickErr := exec.Command("git", "-C", dir, "cherry-pick", c).CombinedOutput()
			if pickErr == nil {
				continue
			}
			// A commit whose content is already upstream applies to nothing and
			// cherry-pick stops with "nothing to commit". That is NOT a
			// conflict — the fleet opens every worktree with a reap-safe anchor
			// commit (FAC-106) that is frequently redundant, so aborting here
			// made every fleet branch unharvestable. Distinguish by asking git
			// whether the tree is actually clean rather than by matching text.
			status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
			if len(strings.TrimSpace(string(status))) == 0 {
				if skipOut, skipErr := exec.Command("git", "-C", dir, "cherry-pick", "--skip").CombinedOutput(); skipErr != nil {
					exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
					return fmt.Errorf("redundant commit %s could not be skipped:\n%s", c[:min(9, len(c))], skipOut)
				}
				fmt.Fprintf(os.Stderr, "herd harvest-merge: skipped %s (already upstream, no content to apply)\n", c[:min(9, len(c))])
				continue
			}
			exec.Command("git", "-C", dir, "cherry-pick", "--abort").Run()
			return fmt.Errorf("cherry-pick %s conflicted, aborting before any PR:\n%s", c[:min(9, len(c))], out)
		}
		// Hard stage gate: a marker in the harvested ADDED lines means the pick
		// produced a structurally broken diff, and once it is in a PR body
		// nobody re-reads it.
		// A hard gate must never pass because its input failed to load: an
		// empty diff from a failed git call would yield zero markers.
		staged, diffErr := exec.Command("git", "-C", dir, "diff", *base+"...HEAD").Output()
		if diffErr != nil {
			return fmt.Errorf("cannot read the harvested diff to gate it: %w", diffErr)
		}
		if markers := harvestmerge.ConflictMarkers(string(staged)); len(markers) > 0 {
			if !*allowMarkers {
				return fmt.Errorf("REFUSED — %d conflict marker(s) in the harvested diff:\n  %s\n"+
					"  If these are fixture CONTENT rather than a broken pick (e.g. a marker parser's\n"+
					"  own test data), re-run with --allow-markers. There was previously no escape\n"+
					"  hatch at all, which made pkg/conflict/conflict_test.go unharvestable by this tool.",
					len(markers), strings.Join(markers, "\n  "))
			}
			// Loud and durable in the output: an override on a merge gate must
			// never be quiet, and the operator must own it explicitly.
			fmt.Fprintf(os.Stderr, "herd harvest-merge: WARNING --allow-markers OVERRIDE: proceeding despite %d conflict marker(s):\n  %s\n",
				len(markers), strings.Join(markers, "\n  "))
		}
		return nil
	}()

	// Cleanup runs on the FAILURE path only — the worktree is deliberately kept
	// on success so the coordinator can push from it.
	cleanup := func() {
		exec.Command("git", "worktree", "remove", "--force", dir).Run()
		exec.Command("git", "branch", "-D", plan.TempBranch).Run()
	}

	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "herd harvest-merge: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("herd harvest-merge: harvested %d commit(s) clean onto %s at %s\n", len(commits), *base, dir)
	fmt.Println("herd harvest-merge: gates passed. Publish and merge is the coordinator's explicit action:")
	fmt.Printf("  git push -u origin %s && gh pr create --title %q\n", plan.TempBranch, *title)
	// The worktree is intentionally KEPT on success: the coordinator pushes
	// from it. Cleanup on success is the caller's, after publishing.
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
