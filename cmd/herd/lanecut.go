package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runLaneCut turns accumulated standing-lane work into ONE bounded candidate on
// a fresh branch cut from current origin/main.
//
// FAC-655: a standing lane is a long-lived process, and it had nowhere to put
// its work except a long-lived branch. Measured on the live fleet, with roughly
// 1,100 commits stranded and unmergeable:
//
//	rebase/platform-ops-rebased    459 ahead   630 behind   (dirty)
//	wt/ux-comber                   255 ahead   795 behind
//	standing/docs-custodian        154 ahead   113 behind
//	chain-indexer-fix2-gate         57 ahead   630 behind
//	wt/scout-planner                 8 ahead   836 behind   (27 dirty files)
//
// None of it could merge, for three compounding reasons: hundreds of commits
// behind main so any rebase is a conflict marathon, no scoping to a single task
// so it cannot be reviewed as a candidate, and no command that turns a lane
// branch into one.
//
// The trick is to stop trying to replay history. Rebasing 459 commits over 630
// is hopeless and produces a conflict per commit. What a reviewer and a merge
// actually need is the NET EFFECT for a bounded set of paths, applied to current
// main -- which usually applies cleanly even when the branch is a year of drift,
// because most of that drift is in files this candidate never touched.
//
// So this reads `git diff <base>...<branch> -- <scope>`, applies it to a fresh
// worktree cut from base, and commits it as one candidate. The lane's branch is
// left completely untouched: this is a non-destructive EXTRACTION, so a failed
// cut costs nothing and the lane can keep working.
func runLaneCut(args []string) error {
	fs := flag.NewFlagSet("lane-cut", flag.ContinueOnError)
	branch := fs.String("branch", "", "lane branch holding the work (required)")
	task := fs.String("task", "", "task ref this candidate closes; recorded in the commit")
	base := fs.String("base", "origin/main", "base to cut the candidate from")
	name := fs.String("name", "", "candidate branch name (default: cut/<task-or-lane>)")
	dryRun := fs.Bool("dry-run", false, "report what would be extracted; create nothing")
	var scope multiFlag
	fs.Var(&scope, "scope", "path to include; repeatable. Required: an unscoped cut is not a bounded candidate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*branch) == "" {
		return fmt.Errorf("--branch is required (the lane branch holding the work)")
	}
	if len(scope) == 0 {
		// An unscoped cut would recreate the same unreviewable blob on a new
		// branch. Bounding it is the entire point.
		return fmt.Errorf("--scope is required at least once: an unscoped cut reproduces the same unbounded branch under a new name, which is what made the work unmergeable in the first place")
	}

	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	git := func(a ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", root}, a...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if _, err := git("rev-parse", "--verify", "-q", *branch+"^{commit}"); err != nil {
		return fmt.Errorf("lane branch %q does not resolve to a commit", *branch)
	}
	if _, err := git("rev-parse", "--verify", "-q", *base+"^{commit}"); err != nil {
		return fmt.Errorf("base %q does not resolve to a commit (fetch first?)", *base)
	}

	// Three-dot: what this branch CHANGED since it diverged, not what main did
	// meanwhile. Two-dot here would try to revert every commit main has landed.
	diffArgs := append([]string{"diff", *base + "..." + *branch, "--"}, []string(scope)...)
	patch, err := git(diffArgs...)
	if err != nil {
		return fmt.Errorf("read scoped diff: %w", err)
	}
	if strings.TrimSpace(patch) == "" {
		return fmt.Errorf("scope %v produces an EMPTY diff against %s: this scope has no work to cut "+
			"(already landed, or the paths are wrong). An empty candidate is refused at review ingest anyway",
			[]string(scope), *base)
	}

	stat, _ := git(append([]string{"diff", "--stat", *base + "..." + *branch, "--"}, []string(scope)...)...)
	fmt.Printf("scope %v against %s:\n%s\n", []string(scope), *base, stat)

	if *dryRun {
		fmt.Println("DRY RUN: no worktree created, no branch written, lane branch untouched")
		return nil
	}

	candidate := strings.TrimSpace(*name)
	if candidate == "" {
		label := strings.TrimSpace(*task)
		if label == "" {
			label = safeReviewSurfacePart(*branch)
		}
		candidate = "cut/" + safeReviewSurfacePart(label)
	}
	if _, err := git("rev-parse", "--verify", "-q", candidate+"^{commit}"); err == nil {
		return fmt.Errorf("candidate branch %q already exists; pass --name to cut a distinct one rather than overwriting a previous cut", candidate)
	}

	dir := filepath.Join(root, ".herd", "worktrees", safeReviewSurfacePart(candidate))
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("worktree %s already exists; remove it or pass --name", dir)
	}
	if out, err := git("worktree", "add", "-q", "-b", candidate, dir, *base); err != nil {
		return fmt.Errorf("create candidate worktree from %s: %v: %s", *base, err, out)
	}
	// Fail closed and leave nothing behind: a half-created candidate is worse
	// than none, because it looks reviewable.
	cleanup := true
	defer func() {
		if cleanup {
			_, _ = git("worktree", "remove", "--force", dir)
			_, _ = git("branch", "-D", candidate)
		}
	}()

	apply := exec.Command("git", "-C", dir, "apply", "--index", "--3way", "-")
	apply.Stdin = strings.NewReader(patch + "\n")
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("the scoped work does not apply cleanly to %s: %v\n%s\n"+
			"This scope genuinely conflicts with what main has landed and needs a human or the owning builder. "+
			"Nothing was left behind; the lane branch %q is untouched",
			*base, err, strings.TrimSpace(string(out)), *branch)
	}

	msg := fmt.Sprintf("cut: bounded candidate from %s", *branch)
	if strings.TrimSpace(*task) != "" {
		msg = fmt.Sprintf("%s: bounded candidate cut from %s", strings.TrimSpace(*task), *branch)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-q", "-m", msg).CombinedOutput(); err != nil {
		return fmt.Errorf("commit candidate: %v: %s", err, strings.TrimSpace(string(out)))
	}
	sha, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read candidate sha: %w", err)
	}
	cleanup = false

	head := strings.TrimSpace(string(sha))
	fmt.Printf("candidate cut: branch=%s sha=%s worktree=%s base=%s from=%s\n", candidate, head, dir, *base, *branch)
	fmt.Printf("next: herd review %s --pool --sha %s\n", candidate, head)
	fmt.Printf("the lane branch %s is UNCHANGED; this was an extraction, not a move\n", *branch)
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("empty --scope value")
	}
	*m = append(*m, v)
	return nil
}
