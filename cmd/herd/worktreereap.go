package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/refname"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/resources"
)

// reapRow is one classified worktree.
type reapRow struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
	Class  string `json:"class"`
	Reason string `json:"reason,omitempty"`
	// Base is the ref the landing classification ran against. The act-time
	// fence re-proves landing against this same ref immediately before
	// removal; an empty Base skips the recheck, which only hand-built rows
	// in tests do.
	Base string `json:"base,omitempty"`
}

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
	var targets reapTargets
	fs.Var(&targets, "target", "exact repository-relative worktree path to inspect (repeatable; bounds the operation)")
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

	var entries []worktreeEntry
	var err error
	if len(targets) > 0 {
		registrations, listErr := reapRegistrationLister(root)
		if listErr != nil {
			return listErr
		}
		selected, selectErr := selectReapTargets(root, registrations, targets)
		if selectErr != nil {
			return selectErr
		}
		entries, err = reapEntryInspector(selected)
	} else {
		entries, err = listWorktreeEntries(root)
	}
	if err != nil {
		return err
	}
	landed, kept := classifyReapEntries(root, *base, *byPR, entries)
	sort.Slice(landed, func(i, j int) bool { return landed[i].Path < landed[j].Path })

	// FAC-681: DO the work before reporting it.
	//
	// The JSON branch used to return here, before the removal loop, while
	// reporting applied=true -- so `--apply --json` claimed success and retired
	// nothing. Reported live: harvest-forge-coverage-integrit-355f96a2 was listed
	// as landed with applied=true and exit 0, and the directory and registration
	// were still there afterwards.
	//
	// And `applied` echoed the FLAG, not the outcome. Even the text path was
	// reporting what was ASKED rather than what HAPPENED. A reaper that says it
	// retired something it did not is worse than one that retires nothing: the
	// caller stops checking.
	var retired, failed []map[string]string
	if *apply {
		retired, failed = retireLanded(root, landed)
	}

	if *asJSON {
		out := map[string]any{
			"landed": landed,
			"kept":   kept,
			// applied reports what actually happened. With --apply it is true only
			// when at least one surface was really retired; without it, false.
			"applied":         *apply && len(retired) > 0,
			"apply_requested": *apply,
			"retired":         retired,
			"failed":          failed,
		}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			return err
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d landed worktree(s) could not be retired", len(failed))
		}
		return nil
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

	for _, f := range failed {
		// Report by exact identity; never silently skip.
		fmt.Fprintf(os.Stderr, "worktree-reap: KEPT %s: %s\n", f["path"], f["error"])
	}
	fmt.Printf("worktree-reap: retired=%d kept=%d\n", len(retired), len(failed))
	if len(failed) > 0 {
		return fmt.Errorf("%d landed worktree(s) could not be retired", len(failed))
	}
	return nil
}

// retireLanded removes each landed surface and reports EXACTLY what happened.
//
// FAC-681: a removal failure is reported as KEPT with git's own message rather
// than counted as retired. Removal is deliberately non-forced: a lock, dirty
// surface, or any other refusal is a safety result, not an escalation prompt.
//
// Removal is verified by looking, not by trusting the exit status: the whole
// defect being fixed here is a command that reported success for work it never
// did, so the confirmation has to be independent of the claim.
func retireLanded(root string, landed []reapRow) (retired, failed []map[string]string) {
	return retireLandedWithInspector(root, landed, resources.LSOFProcessInspector{Timeout: 2 * time.Second, MaxOutputBytes: 1 << 20})
}

func retireLandedWithInspector(root string, landed []reapRow, inspector resources.ProcessInspector) (retired, failed []map[string]string) {
	for _, l := range landed {
		err := retireLandedOneWithInspector(root, l, runReapGit, inspector)
		if err != nil {
			failed = append(failed, map[string]string{
				"path":   l.Path,
				"branch": l.Branch,
				"error":  err.Error(),
			})
			continue
		}
		retired = append(retired, map[string]string{"path": l.Path, "branch": l.Branch})
	}
	return retired, failed
}

type reapGitRunner func(root string, args ...string) ([]byte, error)

type reapTargets []string

func (t *reapTargets) String() string { return strings.Join(*t, ",") }

func (t *reapTargets) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("worktree-reap: --target must not be empty")
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("worktree-reap: --target must be repository-relative: %q", value)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("worktree-reap: --target escapes the repository: %q", value)
	}
	*t = append(*t, clean)
	return nil
}

func selectReapTargets(root string, entries []worktreeEntry, targets reapTargets) ([]worktreeEntry, error) {
	if len(targets) == 0 {
		return entries, nil
	}
	absRoot, err := canonicalWorktreePath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	byPath := make(map[string]worktreeEntry, len(entries))
	for _, entry := range entries {
		path, err := canonicalWorktreePath(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree %q: %w", entry.Path, err)
		}
		byPath[filepath.Clean(path)] = entry
	}
	selected := make([]worktreeEntry, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		path := filepath.Clean(filepath.Join(absRoot, target))
		path, err = canonicalWorktreePath(path)
		if err != nil {
			return nil, fmt.Errorf("resolve target %q: %w", target, err)
		}
		entry, ok := byPath[path]
		if !ok {
			return nil, fmt.Errorf("worktree-reap: --target %q is not a registered worktree", target)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		selected = append(selected, entry)
	}
	return selected, nil
}

func runReapGit(root string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
}

// retireLandedOne makes worktree and branch retirement one reported outcome.
// The caller has already proved the patch landed. If branch deletion fails
// after the required worktree removal, the still-existing branch is used to
// restore the exact worktree path before failure is returned.
func retireLandedOne(root string, l reapRow, run reapGitRunner) error {
	return retireLandedOneWithInspector(root, l, run, noOpReapProcessInspector{})
}

type noOpReapProcessInspector struct{}

func (noOpReapProcessInspector) InUse(context.Context, string) (resources.ProcessUsage, error) {
	return resources.ProcessUsage{}, nil
}

func retireLandedOneWithInspector(root string, l reapRow, run reapGitRunner, inspector resources.ProcessInspector) error {
	if strings.TrimSpace(l.Head) == "" {
		return fmt.Errorf("retire %s: observed worktree HEAD is required", l.Path)
	}
	ref := "refs/heads/" + strings.TrimSpace(l.Branch)
	observed, observeErr := run(root, "rev-parse", "--verify", ref)
	if observeErr != nil || strings.TrimSpace(string(observed)) != strings.TrimSpace(l.Head) {
		return fmt.Errorf("retire %s: branch identity changed or unreadable (observed=%q want=%q error=%v)",
			l.Path, strings.TrimSpace(string(observed)), strings.TrimSpace(l.Head), observeErr)
	}
	// Classification and act are separate processes in the CLI lifecycle. A
	// writer can acquire the surface, add evidence, or lock it after the first
	// scan. Re-read the native worktree state immediately before removal and
	// refuse every changed or uncertain identity; this is the act-time fence.
	entries, listErr := listWorktreeEntries(root)
	if listErr != nil {
		return fmt.Errorf("retire %s: act-time worktree revalidation failed: %w", l.Path, listErr)
	}
	current, found := exactWorktreeEntry(entries, l.Path)
	if !found {
		return fmt.Errorf("retire %s: act-time worktree identity disappeared", l.Path)
	}
	if current.Head != l.Head || current.Branch != l.Branch || current.Detached || current.IsMain {
		return fmt.Errorf("retire %s: act-time worktree identity changed (head=%q branch=%q)", l.Path, current.Head, current.Branch)
	}
	if current.StatusError != "" {
		return fmt.Errorf("retire %s: act-time worktree status is unknown: %s", l.Path, current.StatusError)
	}
	if current.Locked {
		return fmt.Errorf("retire %s: act-time worktree is locked: %s", l.Path, current.LockReason)
	}
	if current.Dirty {
		return fmt.Errorf("retire %s: act-time worktree has uncommitted, untracked, or ignored content", l.Path)
	}
	if isResidentHome(current.Branch, current.Path) {
		return fmt.Errorf("retire %s: act-time worktree is a protected resident home", l.Path)
	}
	// FAC-805: classification and act are separate processes, so the landing
	// itself must be re-proven immediately before removal against the same
	// base the classification used. A squash that was reverted, a base that
	// was force-moved, or any proof refusal keeps the worktree; receipt files
	// and stale classifications alone never authorize deletion.
	if strings.TrimSpace(l.Base) != "" {
		ahead := commitsAhead(root, l.Base, l.Branch)
		if ahead != 0 {
			if _, err := rangeLandedProof(root, l.Base, l.Branch); err != nil {
				return fmt.Errorf("retire %s: act-time landing recheck against %s failed (ahead=%d): %v",
					l.Path, l.Base, ahead, err)
			}
		}
	}
	ownerCtx, cancelOwner := context.WithTimeout(context.Background(), 2*time.Second)
	usage, ownerErr := inspector.InUse(ownerCtx, current.Path)
	cancelOwner()
	if ownerErr != nil {
		return fmt.Errorf("retire %s: act-time owner census failed: %w", l.Path, ownerErr)
	}
	if usage.MetadataUnavailable {
		return fmt.Errorf("retire %s: act-time owner census is incomplete", l.Path)
	}
	if usage.CWD || usage.OpenFile || usage.ReferencedPath {
		return fmt.Errorf("retire %s: act-time owner census found active use (cwd=%t open=%t referenced=%t pids=%v)", l.Path, usage.CWD, usage.OpenFile, usage.ReferencedPath, usage.PIDs)
	}

	// Never force removal: Git's normal operation is the final safety check and
	// refuses a worktree that became dirty or locked after our revalidation.
	out, err := run(root, "worktree", "remove", l.Path)
	if err == nil && worktreeExists(l.Path) {
		err = fmt.Errorf("git reported success but worktree still exists")
	}
	if err != nil {
		return fmt.Errorf("remove worktree %s: %s (%w)", l.Path, strings.TrimSpace(string(out)), err)
	}

	// Compare-and-delete the exact ref observed during classification. A plain
	// branch -D can delete a replacement commit that appeared after the
	// patch-landed proof.
	branchOut, branchErr := run(root, "update-ref", "-d", ref, strings.TrimSpace(l.Head))
	verifyOut, verifyErr := run(root, "show-ref", "--verify", "--quiet", ref)
	if branchErr == nil && verifyErr != nil {
		return nil
	}
	if branchErr == nil {
		branchErr = fmt.Errorf("compare-and-delete reported success but ref remains at %s", strings.TrimSpace(string(verifyOut)))
	}

	// Compensate the partial mutation. The branch is the durable copy and still
	// exists when deletion fails, so restore the exact surface rather than
	// reporting a half-retirement as success.
	restoreOut, restoreErr := run(root, "worktree", "add", "--", l.Path, l.Branch)
	if restoreErr == nil && !worktreeExists(l.Path) {
		restoreErr = fmt.Errorf("git reported success but restored worktree is absent")
	}
	if restoreErr != nil {
		return fmt.Errorf("delete branch %s after worktree removal: %s (%v); compensation failed: %s (%v)",
			l.Branch, strings.TrimSpace(string(branchOut)), branchErr,
			strings.TrimSpace(string(restoreOut)), restoreErr)
	}
	return fmt.Errorf("delete branch %s after worktree removal: %s (%w); worktree restored at %s",
		l.Branch, strings.TrimSpace(string(branchOut)), branchErr, l.Path)
}

func exactWorktreeEntry(entries []worktreeEntry, path string) (worktreeEntry, bool) {
	want, err := canonicalWorktreePath(path)
	if err != nil {
		return worktreeEntry{}, false
	}
	want = filepath.Clean(want)
	for _, entry := range entries {
		got, err := canonicalWorktreePath(entry.Path)
		if err == nil && filepath.Clean(got) == want {
			return entry, true
		}
	}
	return worktreeEntry{}, false
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else if parent, parentErr := filepath.EvalSymlinks(filepath.Dir(abs)); parentErr == nil {
		abs = filepath.Join(parent, filepath.Base(abs))
	}
	return filepath.Clean(abs), nil
}

// isResidentHome reports whether a worktree is a standing lane's home rather
// than a task worktree. Both look "landed" because a home tracks the base.
func isResidentHome(branch, path string) bool {
	b := strings.ToLower(strings.TrimSpace(branch))
	if refname.IsStandingBranch(b) || b == "main" || b == "master" {
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
	Path        string
	Branch      string
	Head        string
	Detached    bool
	Locked      bool
	LockReason  string
	Dirty       bool
	StatusError string
	IsMain      bool
}

func listWorktreeEntries(root string) ([]worktreeEntry, error) {
	entries, err := listWorktreeRegistrations(root)
	if err != nil {
		return nil, err
	}
	return inspectWorktreeEntries(entries), nil
}

// listWorktreeRegistrations reads only Git's registration metadata. It does
// not inspect any registered worktree, so target-bounded reaping can validate
// selectors before touching unselected paths.
func listWorktreeRegistrations(root string) ([]worktreeEntry, error) {
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
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
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

// inspectWorktreeEntries performs the expensive status inspection for the
// already-selected registrations. Main and detached surfaces never required
// status for classification, matching the historical full-sweep behavior.
func inspectWorktreeEntries(entries []worktreeEntry) []worktreeEntry {
	for index := range entries {
		entry := &entries[index]
		if entry.IsMain || entry.Detached {
			continue
		}
		// Include ignored files: generated evidence/cache content is part of
		// the safety boundary even when ordinary status hides it.
		status, statusErr := reapStatusRunner(entry.Path, "status", "--porcelain", "--untracked-files=all", "--ignored")
		if statusErr != nil {
			entry.StatusError = statusErr.Error()
		} else {
			entry.Dirty = len(strings.TrimSpace(status)) > 0
		}
	}
	return entries
}

var (
	reapRegistrationLister = listWorktreeRegistrations
	reapEntryInspector     = func(entries []worktreeEntry) ([]worktreeEntry, error) {
		return inspectWorktreeEntries(entries), nil
	}
	reapStatusRunner = gitOutIn
)

func gitOutIn(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git -C %s %s: %w: %s", dir, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// classifyReapEntries sorts every entry into landed or kept. The split is
// conservative by construction: a row is landed only when its work is
// provably on the base, and every uncertain, dirty, locked, detached, or
// unanswerable surface is kept with its exact identity.
func classifyReapEntries(root, base string, byPR bool, entries []worktreeEntry) (landed, kept []reapRow) {
	for _, e := range entries {
		r := reapRow{Path: e.Path, Branch: e.Branch, Head: e.Head, Base: base}
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
		case e.StatusError != "":
			r.Class, r.Reason = "unknown", "status inspection failed: "+e.StatusError
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
			if byPR {
				if closed, why := prClosedAndLanded(root, e.Branch, base); closed {
					r.Class, r.Reason = "landed", why
					landed = append(landed, r)
					continue
				} else if why != "" {
					r.Class, r.Reason = "pr-open", why
					kept = append(kept, r)
					continue
				}
			}
			ahead := commitsAhead(root, base, e.Branch)
			switch {
			case ahead < 0:
				r.Class, r.Reason = "unknown", "could not compare against "+base
			case ahead == 0:
				r.Class, r.Reason = "landed", "no unique commits against "+base+"; removal is lossless"
			default:
				// FAC-805: ahead counts commits, but a squash-merge collapses
				// the whole reviewed range into one base commit, so per-commit
				// patch identity cannot survive it -- git cherry reports the
				// landed branch's every commit as unique and this reaper
				// called merged harvests (PR803, PR804) unmerged. Before
				// declaring unmerged work, ask the coordinator's tested
				// whole-range proof whether the branch's content is on the
				// base: squash-range patch identity plus exact replay tree.
				// Every proof refusal and every failed lookup keeps the
				// worktree; the proof can only ever add a "landed", never
				// remove a guard.
				if proof, proofErr := rangeLandedProof(root, base, e.Branch); proofErr == nil {
					r.Class, r.Reason = "landed", fmt.Sprintf(
						"whole range is in %s via %s (merge %s); per-commit uniqueness is meaningless across a squash",
						base, proof.Method, shortSha(proof.MergeSHA))
				} else if _, resolveErr := resolveReapRefs(root, base, e.Branch); resolveErr != nil {
					r.Class, r.Reason = "unknown", fmt.Sprintf("git lookup failed against %s: %v", base, resolveErr)
				} else {
					r.Class = "unmerged"
					r.Reason = fmt.Sprintf("%d unique commit(s) not in %s (whole-range proof refused: %v); unmerged work is not garbage", ahead, base, proofErr)
				}
			}
		}
		if r.Class == "landed" {
			landed = append(landed, r)
		} else {
			kept = append(kept, r)
		}
	}
	return landed, kept
}

// rangeLandedProof reuses the coordinator's tested whole-range landing proof
// (FAC-805): it asks whether the branch's combined base..branch delta and its
// replayed result tree are genuinely present on the base history, which is the
// question a squash-merge leaves answerable when per-commit patch ids are not.
// The base for the proof is the branch's own merge-base with the base ref, so
// a base that advanced after the merge (PR803 then PR804 on one main) still
// proves the reviewed range against the base it was reviewed on.
//
// A historical proof alone is not reap authority: a landing that main later
// REVERTED still proves at its merge point while its net content is gone.
// Retirement must be lossless at the CURRENT tip, so the branch's delta is
// replayed onto the base tip with the same native merge-tree primitive the
// proof uses internally, and the result must reproduce the base tip's tree
// exactly. A conflicting, changing, or failed replay keeps the worktree.
func rangeLandedProof(root, base, branch string) (*mergeadmit.Proof, error) {
	mergeBase, err := resolveReapRefs(root, base, branch)
	if err != nil {
		return nil, err
	}
	proof, err := mergeadmit.ProveEquivalentLanded(root, mergeadmit.ProofRequest{
		BaseSHA:      mergeBase,
		CandidateSHA: branch,
		LandedSHA:    base,
	})
	if err != nil {
		return nil, err
	}
	tipTree, err := reapGitTree(root, base)
	if err != nil {
		return nil, err
	}
	replay, err := exec.Command("git", "-C", root, "merge-tree", "--write-tree",
		"--merge-base", mergeBase, base, branch).Output()
	if err != nil {
		// Exit 1 is a conflicted replay: the delta does not apply cleanly to
		// the current tip, so containment is unproven. Any other status is a
		// failed lookup. Both keep the worktree.
		return nil, fmt.Errorf("merge-tree replay of %s onto %s did not prove containment: %w", branch, base, err)
	}
	replayed := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(replay)), "\n", 2)[0])
	if replayed != tipTree {
		return nil, fmt.Errorf("branch %s replays onto %s as tree %s, but the tip is %s; net content is not present now",
			branch, base, shortSha(replayed), shortSha(tipTree))
	}
	return proof, nil
}

// reapGitTree resolves a commit-ish to its tree object id. A failed lookup is
// an error, never an empty string that could compare equal to another empty.
func reapGitTree(root, ref string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", ref+"^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("resolve tree of %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveReapRefs resolves the proof's three points and returns the merge-base
// of the branch with the base ref. A failed lookup is an error, never a
// false "not landed": an unanswerable question must not read as garbage.
func resolveReapRefs(root, base, branch string) (string, error) {
	for _, ref := range []string{base, branch} {
		if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", ref+"^{commit}").Run(); err != nil {
			return "", fmt.Errorf("resolve %s: %w", ref, err)
		}
	}
	out, err := exec.Command("git", "-C", root, "merge-base", base, branch).Output()
	if err != nil {
		return "", fmt.Errorf("merge-base %s %s: %w", base, branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
