package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// runWorktreeReap retires worktrees whose work has demonstrably LANDED.
//
// FAC-672: worktree creation is automatic and retirement is not, so the two
// were never one lifecycle. Measured on the live repository, 403 registrations:
//
//	110  detached review-pool surfaces (transient, correct)
//	 73  on a branch with NO unique commits -- the work is already in main
//	116  on a branch with unique commits that merges CLEAN -- stranded, mergeable
//	101  on a branch that conflicts -- needs a builder
//
// The 73 are the pure leak: their work landed and nothing removed them. They
// accumulate forever, and at roughly a quarter-gigabyte each that is how the
// disk balloons without any single actor doing anything wrong.
//
// This deliberately does NOT touch the 116 or the 101. Unmerged work is not
// garbage, and a reaper that removes it to reclaim space trades correctness for
// capacity -- the operator excluded exactly that class earlier and was right to.
// Only "the commits are in main" makes a worktree removable, because only then
// is removal provably lossless.
//
// Everything it declines is reported by exact identity. A silent skip is how a
// stale worktree becomes invisible instead of actionable.
func runWorktreeReap(args []string) error {
	fs := flag.NewFlagSet("worktree-reap", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "remove the landed worktrees; without it, report only")
	asJSON := fs.Bool("json", false, "emit the classification as JSON")
	base := fs.String("base", "origin/main", "ref that defines 'landed'")
	// FAC-673: retire by the PR's own closure, not only by branch state.
	//
	// worktree-reap alone is a SWEEP: it must be run, and between runs the leak
	// accumulates. A launch receipt that records its PR turns retirement into a
	// lifecycle transition -- the PR closed, the patch is verifiably in base,
	// therefore the surface is spent. The verification is the point: a closed PR
	// whose patch did NOT land is abandoned work, not finished work, and
	// retiring it would destroy the only copy.
	byPR := fs.Bool("by-pr", false,
		"also retire worktrees whose recorded PR is closed AND whose patch is verifiably in base")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")

	entries, err := listWorktreeEntries(root)
	if err != nil {
		return err
	}
	type row struct {
		Path   string `json:"path"`
		Branch string `json:"branch,omitempty"`
		Class  string `json:"class"`
		Reason string `json:"reason,omitempty"`
	}
	var landed, kept []row
	for _, e := range entries {
		r := row{Path: e.Path, Branch: e.Branch}
		switch {
		case e.IsMain:
			r.Class, r.Reason = "main", "the repository's own checkout"
		case e.Detached:
			// A pool slot or review surface. Its identity is a lease, not a
			// branch, and reclaiming it belongs to the pool, not here.
			r.Class, r.Reason = "detached", "detached surface; reclaimed by the review pool, not by branch state"
		case e.Locked:
			r.Class, r.Reason = "locked", "locked: "+e.LockReason
		case e.Branch == "":
			r.Class, r.Reason = "unknown", "no branch and not detached; unresolved state, left alone"
		case e.Dirty:
			r.Class, r.Reason = "dirty", "uncommitted changes would be destroyed"
		case isResidentHome(e.Branch, e.Path):
			// FAC-672: a standing lane's RESIDENT HOME tracks main and therefore
			// has no unique commits, which makes it look landed. It is not a task
			// worktree: removing it evicts a live lane from the directory it
			// works in.
			//
			// Caught in dry run before any --apply: the coordinator's own home
			// (standing/orchestrator) was classified removable. A reaper that
			// takes out the coordinator is worse than one that reclaims nothing,
			// and "no unique commits" is exactly the signal that cannot tell the
			// two apart on its own.
			r.Class, r.Reason = "resident-home", "standing lane home; tracks base by design and is not a task worktree"
		default:
			if *byPR {
				if closed, why := prClosedAndLanded(root, e.Branch, *base); closed {
					r.Class, r.Reason = "landed", why
					landed = append(landed, r)
					continue
				} else if why != "" {
					r.Class, r.Reason = "pr-open", why
					kept = append(kept, r)
					continue
				}
			}
			ahead := commitsAhead(root, *base, e.Branch)
			switch {
			case ahead < 0:
				r.Class, r.Reason = "unknown", "could not compare against "+*base
			case ahead == 0:
				r.Class, r.Reason = "landed", "no unique commits against "+*base+"; removal is lossless"
			default:
				r.Class = "unmerged"
				r.Reason = fmt.Sprintf("%d unique commit(s) not in %s; unmerged work is not garbage", ahead, *base)
			}
		}
		if r.Class == "landed" {
			landed = append(landed, r)
		} else {
			kept = append(kept, r)
		}
	}
	sort.Slice(landed, func(i, j int) bool { return landed[i].Path < landed[j].Path })

	if *asJSON {
		out := map[string]any{"landed": landed, "kept": kept, "applied": *apply}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	byClass := map[string]int{}
	for _, k := range kept {
		byClass[k.Class]++
	}
	fmt.Printf("worktree-reap: %d registrations\n", len(entries))
	fmt.Printf("  landed (removable): %d\n", len(landed))
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		fmt.Printf("  %-18s: %d\n", c, byClass[c])
	}
	// FAC-676: name lanes taking surfaces faster than they finish them. This is
	// the accumulation MECHANISM, distinct from the retirement leak above.
	if lanes := configuredLaneNames(root); len(lanes) > 0 {
		if over := laneAllocations(entries, lanes); len(over) > 0 {
			fmt.Printf("\nlanes holding more than one task worktree (contract is one + resident home): %d\n", len(over))
			for _, a := range over[:minInt(6, len(over))] {
				fmt.Printf("  %-26s %d task worktrees (+%d over)\n", a.Lane, len(a.TaskPaths), a.Excess)
			}
			fmt.Println("  these are REPORTED, never reaped: the extras may hold real unmerged work.")
			fmt.Println("  merge or close them; that is a decision about work, not about capacity.")
		}
	}

	if !*apply {
		fmt.Println("DRY RUN: pass --apply to retire the landed worktrees")
		for _, l := range landed[:minInt(5, len(landed))] {
			fmt.Printf("  would retire %s (%s)\n", l.Path, l.Branch)
		}
		return nil
	}

	removed, failed := 0, 0
	for _, l := range landed {
		if out, err := exec.Command("git", "-C", root, "worktree", "remove", "--force", l.Path).CombinedOutput(); err != nil {
			// Report by exact identity; never silently skip.
			fmt.Fprintf(os.Stderr, "worktree-reap: KEPT %s: %s\n", l.Path, strings.TrimSpace(string(out)))
			failed++
			continue
		}
		// The branch is fully merged, so deleting it is lossless too. -d refuses
		// anything unmerged, which is a second independent check on top of ours.
		_, _ = exec.Command("git", "-C", root, "branch", "-d", l.Branch).CombinedOutput()
		removed++
	}
	fmt.Printf("worktree-reap: retired=%d kept=%d\n", removed, failed)
	return nil
}

// isResidentHome reports whether a worktree is a standing lane's home rather
// than a task worktree. Both look "landed" because a home tracks the base.
func isResidentHome(branch, path string) bool {
	b := strings.ToLower(strings.TrimSpace(branch))
	if strings.HasPrefix(b, "standing/") || b == "main" || b == "master" {
		return true
	}
	// A checkout that sits directly beside the repository rather than inside a
	// managed pool is somebody's working directory, not a task surface.
	base := strings.ToLower(filepath.Base(strings.TrimSuffix(path, "/")))
	for _, marker := range []string{"orchestrator", "supervisor", "herd-smith", "coordinator"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return false
}

// prClosedAndLanded reports whether this branch's recorded PR has closed AND its
// patch is verifiably in base.
//
// Both halves are required. A closed PR whose patch did NOT land is ABANDONED
// work, not finished work, and its worktree may hold the only copy -- retiring
// it on closure alone would destroy it. Patch identity rather than ancestry is
// used deliberately: a rebase-merge or squash changes the SHA, so ancestry
// reports a false negative for work that genuinely landed (verified earlier in
// this session that patch-id is stable across a clean rebase).
//
// The second return explains a decline so a caller can report it by identity;
// empty means "no recorded PR for this branch", which is simply not this
// function's business.
func prClosedAndLanded(root, branch, base string) (bool, string) {
	receipts, err := launch.ReadReceipts(launch.ReceiptPathFor(root))
	if err != nil {
		return false, ""
	}
	var pr string
	for i := len(receipts) - 1; i >= 0; i-- {
		if strings.TrimSpace(receipts[i].Branch) == branch && strings.TrimSpace(receipts[i].PullRequest) != "" {
			pr = strings.TrimSpace(receipts[i].PullRequest)
			break
		}
	}
	if pr == "" {
		return false, ""
	}
	state := strings.ToUpper(strings.TrimSpace(ghPRState(root, pr)))
	if state == "" {
		return false, fmt.Sprintf("PR %s state is unknown; not retiring on an unreadable answer", pr)
	}
	if state == "OPEN" {
		return false, fmt.Sprintf("PR %s is still open", pr)
	}
	if !patchIsInBase(root, branch, base) {
		// Closed but not landed. This is the case that makes verification
		// mandatory rather than decorative.
		return false, fmt.Sprintf("PR %s is %s but its patch is NOT in %s; abandoned work, kept", pr, state, base)
	}
	return true, fmt.Sprintf("PR %s is %s and its patch is verifiably in %s", pr, state, base)
}

func ghPRState(root, pr string) string {
	out, err := exec.Command("gh", "pr", "view", pr, "--json", "state", "--jq", ".state").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// patchIsInBase reports whether the branch's net change is already present in
// base, by patch identity rather than commit ancestry.
func patchIsInBase(root, branch, base string) bool {
	if commitsAhead(root, base, branch) == 0 {
		return true
	}
	out, err := exec.Command("git", "-C", root, "cherry", base, branch).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+") {
			return false // at least one patch is not upstream
		}
	}
	return true
}

// configuredLaneNames reads the lane roster this repository declares, so lane
// attribution comes from configuration rather than a hardcoded list that would
// go stale the moment a lane is added.
func configuredLaneNames(root string) []string {
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil || cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Lanes))
	for _, l := range cfg.Lanes {
		if n := strings.TrimSpace(l.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

type worktreeEntry struct {
	Path       string
	Branch     string
	Detached   bool
	Locked     bool
	LockReason string
	Dirty      bool
	IsMain     bool
}

func listWorktreeEntries(root string) ([]worktreeEntry, error) {
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	absRoot, _ := filepath.Abs(root)
	var entries []worktreeEntry
	var cur *worktreeEntry
	flush := func() {
		if cur == nil {
			return
		}
		if abs, err := filepath.Abs(cur.Path); err == nil && abs == absRoot {
			cur.IsMain = true
		}
		if !cur.IsMain && !cur.Detached {
			cur.Dirty = len(strings.TrimSpace(gitOutIn(cur.Path, "status", "--porcelain"))) > 0
		}
		entries = append(entries, *cur)
		cur = nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeEntry{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case strings.HasPrefix(line, "locked"):
			cur.Locked = true
			cur.LockReason = strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		}
	}
	flush()
	return entries, nil
}

func gitOutIn(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// commitsAhead returns how many commits branch has that base does not, or -1
// when the comparison cannot be made. -1 is deliberately NOT zero: an
// unanswerable question must never read as "nothing unique here".
func commitsAhead(root, base, branch string) int {
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", base+".."+branch).Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range strings.TrimSpace(string(out)) {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
