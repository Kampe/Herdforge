package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ReapClass is the fail-closed classification of a worktree before any
// destructive GC action (FAC-117).
type ReapClass string

const (
	// ReapClassRoot is the shared repository checkout; never reaped.
	ReapClassRoot ReapClass = "root"
	// ReapClassProtected is main/master/detached/non-herd; never reaped by auto-GC.
	ReapClassProtected ReapClass = "protected"
	// ReapClassDirty has uncommitted changes; refuse and preserve.
	ReapClassDirty ReapClass = "dirty"
	// ReapClassUnique has committed patches not on the integration base; refuse.
	ReapClassUnique ReapClass = "unique-committed"
	// ReapClassContentMerged has no unique patches vs integration base and is clean.
	ReapClassContentMerged ReapClass = "content-merged"
	// ReapClassUnknown means a Git (or probe) error prevented safe classification.
	// Unknown is a hard refusal — never permission to reap.
	ReapClassUnknown ReapClass = "unknown"
)

// SalvageRefPrefix is the durable refs namespace written before a successful reap
// so the candidate tip remains recoverable after the working tree is removed.
const SalvageRefPrefix = "refs/herd/salvage/"

// LeaseProbe optionally reports whether a worktree still has an active lease or
// session. When the probe returns true or an error, classification is refused.
// Leave nil when no lease subsystem is wired; GC then relies on Git evidence only.
type LeaseProbe func(ctx context.Context, path, branch string) (active bool, err error)

// ReapPolicy controls planning and optional automatic removal (FAC-117).
// Default zero-value is report/dry-run only (AutoReap=false).
type ReapPolicy struct {
	DefaultBranch string
	// AutoReap must be true for any destructive removal. Dry-run and action
	// share the same Classify path and just-in-time revalidation.
	AutoReap bool
	// TargetPaths, when non-empty, restricts consideration to exact path matches
	// (after Abs/Clean). Siblings are never pruned as a side effect.
	TargetPaths []string
	// LeaseProbe is optional active-lease fencing.
	LeaseProbe LeaseProbe
}

// ReapCandidate is one classified worktree with evidence and preservation guidance.
type ReapCandidate struct {
	Path           string
	Branch         string
	HEAD           string
	Class          ReapClass
	UniqueSHAs     []string
	Reason         string
	PreserveAction string
	SalvageRef     string
	SalvageOK      bool
	Integration    string // resolved integration base used for git cherry
	// Eligible is true only when Class is content-merged and salvage is verified
	// (or will be verified immediately before removal).
	Eligible bool
}

// ReapReport is the precomputed candidate set used for both dry-run and action.
type ReapReport struct {
	Candidates []ReapCandidate
	Eligible   []ReapCandidate // subset that may be removed under AutoReap
	Refused    []ReapCandidate
	// Reaped paths actually removed (empty on dry-run).
	Reaped []string
	// Errors are non-fatal per-candidate classification issues already reflected
	// as ReapClassUnknown; a hard list failure is returned as error from Plan/Reap.
	Errors []string
}

// PlanReap classifies every worktree under fail-closed rules without removing
// anything. Dry-run and AutoReap share this precompute step (FAC-117).
func (w *WorktreeManager) PlanReap(ctx context.Context, policy ReapPolicy) (*ReapReport, error) {
	if policy.DefaultBranch == "" {
		policy.DefaultBranch = DefaultBranch
	}

	wtList, err := w.ListWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("reap plan: list worktrees: %w", err)
	}

	integration, err := w.resolveIntegrationBase(ctx, policy.DefaultBranch)
	// Integration resolution failure does not abort the plan: every candidate
	// becomes UNKNOWN so nothing is eligible.
	integrationErr := err

	report := &ReapReport{}
	for _, wt := range wtList {
		if wt == nil {
			continue
		}
		if len(policy.TargetPaths) > 0 {
			matched := false
			for _, tp := range policy.TargetPaths {
				if sameWorktreePath(wt.Path, tp) {
					matched = true
					break
				}
			}
			if !matched {
				// Exact-target mode: siblings are not considered at all.
				continue
			}
		}

		c := w.classifyOne(ctx, wt, policy, integration, integrationErr)
		report.Candidates = append(report.Candidates, c)
		if c.Eligible {
			report.Eligible = append(report.Eligible, c)
		} else {
			report.Refused = append(report.Refused, c)
		}
		if c.Class == ReapClassUnknown && c.Reason != "" {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", c.Path, c.Reason))
		}
	}

	// Deterministic ordering for stable reports and tests.
	sort.Slice(report.Candidates, func(i, j int) bool {
		return report.Candidates[i].Path < report.Candidates[j].Path
	})
	sort.Slice(report.Eligible, func(i, j int) bool {
		return report.Eligible[i].Path < report.Eligible[j].Path
	})
	sort.Slice(report.Refused, func(i, j int) bool {
		return report.Refused[i].Path < report.Refused[j].Path
	})
	return report, nil
}

// Reap executes PlanReap and, when policy.AutoReap is true, removes only the
// eligible set after just-in-time revalidation. Unique, dirty, protected, root,
// and unknown worktrees are never removed.
func (w *WorktreeManager) Reap(ctx context.Context, policy ReapPolicy) (*ReapReport, error) {
	report, err := w.PlanReap(ctx, policy)
	if err != nil {
		return nil, err
	}
	if !policy.AutoReap {
		return report, nil
	}

	// Snapshot eligible paths from the precomputed set; revalidate each one
	// immediately before removal so a concurrent commit cannot be lost.
	for _, planned := range report.Eligible {
		// JIT revalidation against current Git state.
		freshList, lerr := w.ListWorktrees(ctx)
		if lerr != nil {
			return report, fmt.Errorf("reap revalidate list: %w", lerr)
		}
		var current *WorktreeInfo
		for _, wt := range freshList {
			if wt != nil && sameWorktreePath(wt.Path, planned.Path) {
				current = wt
				break
			}
		}
		if current == nil {
			// Already gone — treat as not reaped by us.
			continue
		}
		integration, ierr := w.resolveIntegrationBase(ctx, policy.DefaultBranch)
		reclass := w.classifyOne(ctx, current, policy, integration, ierr)
		if !reclass.Eligible {
			report.Refused = append(report.Refused, reclass)
			report.Errors = append(report.Errors,
				fmt.Sprintf("%s: JIT revalidation refused reap: %s (%s)", reclass.Path, reclass.Class, reclass.Reason))
			continue
		}

		// Ensure durable salvage ref points at the tip and verifies before remove.
		if err := w.ensureSalvageRef(ctx, reclass.SalvageRef, reclass.HEAD); err != nil {
			report.Refused = append(report.Refused, ReapCandidate{
				Path:           reclass.Path,
				Branch:         reclass.Branch,
				HEAD:           reclass.HEAD,
				Class:          ReapClassUnknown,
				Reason:         err.Error(),
				PreserveAction: "keep worktree; salvage ref verification failed",
				SalvageRef:     reclass.SalvageRef,
			})
			report.Errors = append(report.Errors, fmt.Sprintf("%s: salvage: %v", reclass.Path, err))
			continue
		}

		if err := w.RemoveWorktree(ctx, reclass.Path); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: remove: %v", reclass.Path, err))
			continue
		}
		report.Reaped = append(report.Reaped, reclass.Path)
	}
	return report, nil
}

// PruneMergedWorktrees is the historical auto-reap entry point used by pkg/gc.
// FAC-117: it only removes worktrees that classify as content-merged under
// fail-closed rules (dirty/unique/unknown/root/protected are refused).
func (w *WorktreeManager) PruneMergedWorktrees(ctx context.Context, defaultBranch string) (int, error) {
	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	report, err := w.Reap(ctx, ReapPolicy{
		DefaultBranch: defaultBranch,
		AutoReap:      true,
	})
	if err != nil {
		return 0, err
	}
	return len(report.Reaped), nil
}

func (w *WorktreeManager) classifyOne(
	ctx context.Context,
	wt *WorktreeInfo,
	policy ReapPolicy,
	integration string,
	integrationErr error,
) ReapCandidate {
	c := ReapCandidate{
		Path:   wt.Path,
		Branch: wt.Branch,
		HEAD:   wt.Commit,
	}

	// Resolve HEAD if list porcelain omitted it.
	if c.HEAD == "" {
		if head, err := w.headAt(ctx, wt.Path); err == nil {
			c.HEAD = head
		}
	}

	// Root checkout is never reaped.
	if err := RejectSharedRoot(w.RepoRoot, wt.Path); err != nil {
		c.Class = ReapClassRoot
		c.Reason = "shared repository root"
		c.PreserveAction = "never reap the primary checkout"
		return c
	}

	branch := strings.TrimSpace(wt.Branch)
	if branch == "" || branch == "main" || branch == "master" || branch == "HEAD" {
		c.Class = ReapClassProtected
		c.Reason = "integration or detached branch"
		c.PreserveAction = "leave protected branch worktree in place"
		return c
	}

	// Auto-GC only considers herd/* task worktrees; other branches are protected.
	if !strings.HasPrefix(branch, "herd/") {
		c.Class = ReapClassProtected
		c.Reason = "non-herd branch outside auto-GC scope"
		c.PreserveAction = "leave non-task worktree in place"
		return c
	}

	c.SalvageRef = SalvageRefFor(branch)

	// Optional active-lease fencing.
	if policy.LeaseProbe != nil {
		active, err := policy.LeaseProbe(ctx, wt.Path, branch)
		if err != nil {
			c.Class = ReapClassUnknown
			c.Reason = fmt.Sprintf("lease probe error: %v", err)
			c.PreserveAction = "keep worktree until lease state is known"
			return c
		}
		if active {
			c.Class = ReapClassProtected
			c.Reason = "active lease or session"
			c.PreserveAction = "wait for lease release before cleanup"
			return c
		}
	}

	// Dirty working tree → refuse.
	dirty, derr := w.isDirty(ctx, wt.Path)
	if derr != nil {
		c.Class = ReapClassUnknown
		c.Reason = fmt.Sprintf("status error: %v", derr)
		c.PreserveAction = "keep worktree until status can be read"
		return c
	}
	if dirty {
		c.Class = ReapClassDirty
		c.Reason = "uncommitted changes present"
		c.PreserveAction = fmt.Sprintf("commit or stash dirty files; durable tip ref %s", c.SalvageRef)
		return c
	}

	// Integration base required for unique-commit detection.
	if integrationErr != nil || integration == "" {
		c.Class = ReapClassUnknown
		if integrationErr != nil {
			c.Reason = fmt.Sprintf("integration base error: %v", integrationErr)
		} else {
			c.Reason = "integration base unresolved"
		}
		c.PreserveAction = "keep worktree until origin/default can be resolved"
		return c
	}
	c.Integration = integration

	// Unique commits via git cherry (patch-id), never rev-list --count (FAC-117).
	unique, uerr := w.uniqueCommits(ctx, wt.Path, integration, branch)
	if uerr != nil {
		c.Class = ReapClassUnknown
		c.Reason = fmt.Sprintf("cherry error: %v", uerr)
		c.PreserveAction = "keep worktree until unique-commit scan succeeds"
		return c
	}
	if len(unique) > 0 {
		c.Class = ReapClassUnique
		c.UniqueSHAs = unique
		c.Reason = fmt.Sprintf("%d unique unmerged commit(s) vs %s", len(unique), integration)
		c.PreserveAction = fmt.Sprintf(
			"do not reap; preserve branch %s and salvage ref %s; integrate or park unique work first",
			branch, c.SalvageRef,
		)
		// Best-effort pin salvage at tip so unique work has a durable name even
		// while the worktree stays (preservation, not removal precondition).
		if c.HEAD != "" {
			if err := w.ensureSalvageRef(ctx, c.SalvageRef, c.HEAD); err == nil {
				c.SalvageOK = true
			}
		}
		return c
	}

	// Content-merged and clean: eligible only after salvage tip is known.
	c.Class = ReapClassContentMerged
	c.Reason = fmt.Sprintf("no unique commits vs %s; working tree clean", integration)
	c.PreserveAction = fmt.Sprintf("safe to remove worktree after verifying salvage ref %s", c.SalvageRef)
	if c.HEAD == "" {
		c.Class = ReapClassUnknown
		c.Reason = "HEAD unresolved for salvage"
		c.PreserveAction = "keep worktree until HEAD is known"
		c.Eligible = false
		return c
	}
	// Verify (or create) salvage during classification so dry-run reports the
	// same eligibility gate that action will require.
	if err := w.ensureSalvageRef(ctx, c.SalvageRef, c.HEAD); err != nil {
		c.Class = ReapClassUnknown
		c.Reason = fmt.Sprintf("salvage ref error: %v", err)
		c.PreserveAction = "keep worktree until salvage ref verifies"
		return c
	}
	c.SalvageOK = true
	c.Eligible = true
	return c
}

// SalvageRefFor returns the durable salvage ref for a branch name.
func SalvageRefFor(branch string) string {
	b := strings.TrimPrefix(strings.ToLower(branch), "refs/heads/")
	b = strings.ReplaceAll(b, " ", "-")
	return SalvageRefPrefix + b
}

func (w *WorktreeManager) resolveIntegrationBase(ctx context.Context, defaultBranch string) (string, error) {
	// Best-effort fetch; offline is OK if origin/<branch> already exists.
	fetch := execCommandContext(ctx, "git", "fetch", "--quiet", "origin", defaultBranch)
	fetch.Dir = w.RepoRoot
	fetchOut, fetchErr := fetch.CombinedOutput()

	originRef := "origin/" + defaultBranch
	if sha, err := w.revParse(ctx, originRef); err == nil && sha != "" {
		return originRef, nil
	}

	// Fall back to local default branch (local merge without push).
	localRef := defaultBranch
	if sha, err := w.revParse(ctx, localRef); err == nil && sha != "" {
		return localRef, nil
	}

	if fetchErr != nil {
		return "", fmt.Errorf("resolve integration base %s: fetch=%v (%s)",
			defaultBranch, fetchErr, strings.TrimSpace(string(fetchOut)))
	}
	return "", fmt.Errorf("resolve integration base: neither %s nor %s resolvable", originRef, localRef)
}

func (w *WorktreeManager) isDirty(ctx context.Context, path string) (bool, error) {
	cmd := execCommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git status --porcelain: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// uniqueCommits returns patch-unique SHAs (git cherry "+") of branch vs base.
// Fail-closed: any git error is returned; callers must not treat error as empty.
func (w *WorktreeManager) uniqueCommits(ctx context.Context, path, base, branch string) ([]string, error) {
	cmd := execCommandContext(ctx, "git", "-C", path, "cherry", base, branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git cherry %s %s: %v (%s)", base, branch, err, strings.TrimSpace(string(out)))
	}
	var unique []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "+ ") {
			unique = append(unique, strings.TrimPrefix(line, "+ "))
		}
	}
	return unique, nil
}

func (w *WorktreeManager) ensureSalvageRef(ctx context.Context, ref, sha string) error {
	if ref == "" || sha == "" {
		return fmt.Errorf("salvage ref and sha are required")
	}
	if err := w.updateRef(ctx, ref, sha); err != nil {
		return err
	}
	got, err := w.revParse(ctx, ref)
	if err != nil {
		return fmt.Errorf("salvage ref verify read: %w", err)
	}
	if got != sha {
		return fmt.Errorf("salvage ref %s verification failed: got %q want %q", ref, got, sha)
	}
	return nil
}

func sameWorktreePath(a, b string) bool {
	if a == b {
		return true
	}
	return normalizePath(a) == normalizePath(b)
}

// normalizePath resolves Abs + symlinks. When the leaf path no longer exists
// (post-reap), it walks parents so macOS /var → /private/var still matches.
func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	dir := abs
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Clean(filepath.Join(resolved, suffix))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs)
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
		dir = parent
	}
}
