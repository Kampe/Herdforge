package harvest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// Integration implements the README contract: a serialized merge pipeline
// that consumes a Harvester (exact-SHA), Verifier (test gate),
// reviewledger.Ledger via Admit (review gate), and Dispatcher
// (board-complete) as dependencies.
//
// Phases:
//  1. Harvest — list worktrees, check unmerged commits via git cherry.
//  2. Review gate — for each unmerged SHA, resolve live AdmissionContext
//     (task/lease/patch/session provenance) and call reviewledger.Admit.
//     Prose, author-family self-verdicts, stale SHAs, and missing
//     structured receipts never admit. The commit message is NOT parsed.
//  3. Merge gate — acquire lock.DirLock (PID-liveness, not timer-only),
//     checkout main, fetch origin/main, ff-only merge, cherry-pick, test
//     gate, push with race retry.
//  4. Post-merge — proof-readback via sync.MergeEvidence, board-complete
//     via Dispatcher, ledger.Consumed (exactly-once admission spent).
//  5. Cleanup — remove fully-merged worktrees with dirty/unique-work
//     protection and session teardown (only after Consumed readback).

type Integration struct {
	Harvester       *Harvester
	Verifier        Verifier             // optional; nil skips test gate
	Dispatcher      Dispatcher           // required for board-complete
	Ledger          *reviewledger.Ledger // required for Admit review gate
	AdmissionSource AdmissionSource      // required; supplies caller-asserted Admit opts
	SessionManager  SessionManager       // optional; nil skips session teardown
	RepoRoot        string
	LockDir         string
	MaxMergeAge     time.Duration
	DryRun          bool
}

// AdmissionContext is the caller-asserted merge context bound into
// reviewledger.Admit. Every field must match a validated ledger verdict
// for the exact candidate SHA; empty fields fail closed inside Admit.
// This is session/claim provenance, never free text from a PR comment.
type AdmissionContext struct {
	Task           string
	Lease          string
	PatchURL       string
	AuthorFamily   string
	AuthorIdentity string
}

// AdmissionSource resolves live claim/lease/session provenance for a
// candidate about to pass the review gate. A nil source or a resolution
// error fails closed — no merge without asserted context.
type AdmissionSource interface {
	ForCandidate(ctx context.Context, sha string, uw UnmergedWork) (AdmissionContext, error)
}

// StaticAdmissionSource returns the same AdmissionContext for every
// candidate. Used by single-candidate runs and hermetic tests.
type StaticAdmissionSource struct {
	Context AdmissionContext
}

// ForCandidate implements AdmissionSource.
func (s StaticAdmissionSource) ForCandidate(_ context.Context, _ string, _ UnmergedWork) (AdmissionContext, error) {
	return s.Context, nil
}

// MapAdmissionSource resolves per-SHA context. Missing keys fail closed.
type MapAdmissionSource map[string]AdmissionContext

// ForCandidate implements AdmissionSource.
func (m MapAdmissionSource) ForCandidate(_ context.Context, sha string, _ UnmergedWork) (AdmissionContext, error) {
	if c, ok := m[sha]; ok {
		return c, nil
	}
	// Also try full-length matches when harvest listed a short SHA and
	// the map was keyed by the full object id (or vice versa).
	for k, c := range m {
		if strings.HasPrefix(k, sha) || strings.HasPrefix(sha, k) {
			return c, nil
		}
	}
	return AdmissionContext{}, fmt.Errorf("no admission context for candidate sha %s", sha)
}

// Verifier is the consumed contract for test-gate verification.
type Verifier interface {
	Execute(ctx context.Context, dir string) (*VerifyResult, error)
}

// VerifyResult mirrors a subset of pkg/verifier.Result.
type VerifyResult struct {
	Passed bool
	Output string
}

// Dispatcher is the consumed contract for board-complete dispatch.
type Dispatcher interface {
	BoardComplete(ctx context.Context, ref string, evidenceSHA string) error
}

// SessionManager is the consumed contract for agent session teardown.
type SessionManager interface {
	Stop(ctx context.Context, branch string) error
}

// IntegrationOption configures an Integration.
type IntegrationOption func(*Integration)

// WithLockDir sets the advisory lock directory.
func WithLockDir(dir string) IntegrationOption {
	return func(i *Integration) { i.LockDir = dir }
}

// WithDryRun enables dry-run mode (no mutations).
func WithDryRun(dry bool) IntegrationOption {
	return func(i *Integration) { i.DryRun = dry }
}

// WithMaxMergeAge sets the maximum time to wait for the merge lock.
func WithMaxMergeAge(d time.Duration) IntegrationOption {
	return func(i *Integration) { i.MaxMergeAge = d }
}

// WithAdmissionSource sets the source of caller-asserted Admit context
// (task, lease generation, patch id, author session provenance).
func WithAdmissionSource(src AdmissionSource) IntegrationOption {
	return func(i *Integration) { i.AdmissionSource = src }
}

// WithSessionManager sets the session manager for cleanup.
func WithSessionManager(sm SessionManager) IntegrationOption {
	return func(i *Integration) { i.SessionManager = sm }
}

// IntegrationResult carries the outcome of the full pipeline for one harvest.
type IntegrationResult struct {
	HarvestResult      *HarvestResult      `json:"harvest_result"`
	ReviewGatedSHAs    []ReviewGateOutcome `json:"review_gated_shas,omitempty"`
	MergedSHAs         []MergeOutcome      `json:"merged_shas,omitempty"`
	BoardCompletedRefs []string            `json:"board_completed_refs,omitempty"`
	CleanedWorktrees   []string            `json:"cleaned_worktrees,omitempty"`
	Errors             []string            `json:"errors,omitempty"`
	// DiskAdvice is the graduated capacity verdict this run executed under
	// (proceed | serialize); ShedWorktrees counts candidates deferred to a
	// later run by soft-pressure serialization — never silently dropped.
	DiskAdvice    string `json:"disk_advice,omitempty"`
	ShedWorktrees int    `json:"shed_worktrees,omitempty"`
}

// ReviewGateOutcome records the result of the review gate for a SHA.
// Reason is the structured Admit rejection reason (diagnostic only);
// Eligible is the sole merge-authority signal.
type ReviewGateOutcome struct {
	SHA      string `json:"sha"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
	Err      string `json:"err,omitempty"`
}

// MergeOutcome records the result of the merge gate for a SHA.
type MergeOutcome struct {
	SHA           string `json:"sha"`
	Branch        string `json:"branch"`
	CherryPicked  bool   `json:"cherry_picked"`
	Conflict      bool   `json:"conflict"`
	Pushed        bool   `json:"pushed"`
	AlreadyMerged bool   `json:"already_merged,omitempty"`
	Err           string `json:"err,omitempty"`
	MergeSHA      string `json:"merge_sha,omitempty"`
}

// NewIntegration creates an Integration with sensible defaults.
// l must be a reviewledger.Ledger; merge admission always goes through Admit.
func NewIntegration(h *Harvester, v Verifier, d Dispatcher, l *reviewledger.Ledger, repoRoot string, opts ...IntegrationOption) *Integration {
	i := &Integration{
		Harvester:   h,
		Verifier:    v,
		Dispatcher:  d,
		Ledger:      l,
		RepoRoot:    repoRoot,
		LockDir:     filepath.Join(repoRoot, ".git", "herd-shared-checkout.lock.d"),
		MaxMergeAge: 5 * time.Minute,
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Run executes the full harvest → review-gate → merge-gate → post-merge → cleanup flow.
func (in *Integration) Run(ctx context.Context) (*IntegrationResult, error) {
	res := &IntegrationResult{}

	// Phase 1: Harvest (read-only: worktree list + unmerged log inspection).
	hr, err := in.Harvester.Harvest(ctx)
	if err != nil {
		return nil, fmt.Errorf("harvest: %w", err)
	}
	res.HarvestResult = hr
	if len(hr.UnmergedWorktrees) == 0 {
		return res, nil
	}

	// Graduated disk gate before ANY mutation — review-gate worktree ops,
	// cherry-picks onto the shared checkout, cleanup (FAC-153). Probes the
	// repo, temp, and EVERY candidate worktree volume: a pool mounted on a
	// different filesystem must not exhaust behind a healthy-looking repo
	// volume. Hard pressure fails closed (never reclaiming space); the soft
	// band sheds the batch to one worktree per run so mutation volume
	// degrades before work stops.
	diskPaths := []string{in.RepoRoot, os.TempDir()}
	for _, uw := range hr.UnmergedWorktrees {
		diskPaths = append(diskPaths, uw.WorktreePath)
	}
	adv := preflight.AdviseDiskPressure("integration", diskPaths...)
	if adv.Verdict == preflight.AdviceRefuse {
		if adv.Evidence != nil {
			return nil, adv.Evidence
		}
		return nil, fmt.Errorf("integration refused: %s", adv.Detail)
	}
	res.DiskAdvice = adv.Verdict
	if adv.Verdict == preflight.AdviceSerialize && len(hr.UnmergedWorktrees) > 1 {
		res.ShedWorktrees = len(hr.UnmergedWorktrees) - 1
		hr.UnmergedWorktrees = hr.UnmergedWorktrees[:1]
	}
	// Common admission/reservation: hold the per-mutation headroom for the
	// whole review/merge/cleanup pipeline so concurrent integrations are
	// bounded by real remaining capacity (FAC-153).
	release, admitErr := preflight.AdmitDiskMutation("integration", diskPaths...)
	if admitErr != nil {
		return nil, admitErr
	}
	defer release()

	// Phase 2: Review gate
	for _, uw := range hr.UnmergedWorktrees {
		for _, sha := range uw.Unmerged {
			outcome := in.runReviewGate(ctx, sha, uw)
			res.ReviewGatedSHAs = append(res.ReviewGatedSHAs, outcome)
		}
	}

	// Track per-worktree merge counts for cleanup eligibility.
	mergedByWorktree := make(map[string]int)
	totalByWorktree := make(map[string]int)
	for _, uw := range hr.UnmergedWorktrees {
		totalByWorktree[uw.WorktreePath] = len(uw.Unmerged)
	}

	// Phase 3+4: Merge gate + post-merge
	for _, rg := range res.ReviewGatedSHAs {
		if !rg.Eligible {
			continue
		}
		if in.DryRun {
			continue
		}
		mo, err := in.runMergeGate(ctx, rg)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("merge %s: %v", rg.SHA, err))
			continue
		}
		res.MergedSHAs = append(res.MergedSHAs, *mo)

		if mo.AlreadyMerged {
			mergedByWorktree[rg.Worktree]++
			continue
		}

		if mo.Pushed && mo.MergeSHA != "" {
			ref := hsync.NormalizeRef(branchToRef(rg.Branch))

			// Phase 4b: Proof readback via MergeEvidence
			proof, err := hsync.MergeEvidence(in.RepoRoot, ref, mo.MergeSHA)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("merge evidence %s: %v", ref, err))
				continue
			}
			if proof == "" {
				res.Errors = append(res.Errors, fmt.Sprintf("no merge evidence for %s", ref))
				continue
			}

			// Phase 4c: Board-complete
			if in.Dispatcher != nil {
				if err := in.Dispatcher.BoardComplete(ctx, ref, mo.MergeSHA); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("board-complete %s: %v", ref, err))
					continue
				}
				res.BoardCompletedRefs = append(res.BoardCompletedRefs, ref)
			}

			// Phase 4d: Ledger consumed
			if in.Ledger != nil {
				if err := in.Ledger.Consumed(rg.SHA, mo.MergeSHA); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("ledger consumed %s: %v", rg.SHA, err))
				}
			}

			mergedByWorktree[rg.Worktree]++
		}
	}

	// Phase 5: Cleanup — only for worktrees where ALL SHAs were merged.
	for _, uw := range hr.UnmergedWorktrees {
		if totalByWorktree[uw.WorktreePath] > 0 && mergedByWorktree[uw.WorktreePath] == totalByWorktree[uw.WorktreePath] {
			cleaned, err := in.runCleanup(ctx, uw)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("cleanup %s: %v", uw.WorktreePath, err))
				continue
			}
			if cleaned {
				res.CleanedWorktrees = append(res.CleanedWorktrees, uw.WorktreePath)
			}
		}
	}

	return res, nil
}

func (in *Integration) runReviewGate(ctx context.Context, sha string, uw UnmergedWork) ReviewGateOutcome {
	outcome := ReviewGateOutcome{
		SHA:      sha,
		Branch:   uw.Branch,
		Worktree: uw.WorktreePath,
	}

	if in.Ledger == nil {
		outcome.Err = "no review ledger configured"
		outcome.Reason = "no review ledger configured"
		return outcome
	}
	if in.AdmissionSource == nil {
		outcome.Err = "no admission context source configured"
		outcome.Reason = "no admission context source configured"
		return outcome
	}

	// Exact-current-candidate gate at the integration tree: the SHA harvest
	// listed must still be a real commit on the worktree branch. A rebase
	// that advanced HEAD to a new object invalidates any prior verdict
	// bound to the old SHA (FAC-126 stale-SHA shape).
	if err := ensureCandidateOnBranch(ctx, uw.WorktreePath, sha); err != nil {
		outcome.Err = err.Error()
		outcome.Reason = "candidate not current on integration tree"
		return outcome
	}

	adm, err := in.AdmissionSource.ForCandidate(ctx, sha, uw)
	if err != nil {
		outcome.Err = err.Error()
		outcome.Reason = "admission context resolution failed"
		return outcome
	}

	result, err := in.Ledger.Admit(reviewledger.AdmissionOpts{
		CandidateSHA:   sha,
		Task:           adm.Task,
		Lease:          adm.Lease,
		PatchURL:       adm.PatchURL,
		AuthorFamily:   adm.AuthorFamily,
		AuthorIdentity: adm.AuthorIdentity,
	})
	// Fail closed: I/O/parse errors never look like "no verdict".
	// Policy refusals return a non-nil result with Admitted=false plus a
	// structured Reason; callers gate solely on Admitted.
	if result != nil && result.Admitted {
		outcome.Eligible = true
		outcome.Reason = result.Reason
		return outcome
	}
	outcome.Eligible = false
	if result != nil && result.Reason != "" {
		outcome.Reason = result.Reason
		outcome.Err = result.Reason
	} else if err != nil {
		outcome.Err = err.Error()
		outcome.Reason = err.Error()
	} else {
		outcome.Err = "admission refused"
		outcome.Reason = "admission refused"
	}
	return outcome
}

// ensureCandidateOnBranch fails closed when sha is not a commit reachable
// from the worktree HEAD. That is the "current integration tree" check:
// advancing/rebasing the branch to a new tip without a fresh verdict
// cannot smuggle an old SHA past the gate via a stale harvest snapshot.
func ensureCandidateOnBranch(ctx context.Context, worktreePath, sha string) error {
	if _, err := gitOutput(ctx, worktreePath, "rev-parse", "--verify", "-q", sha+"^{commit}"); err != nil {
		return fmt.Errorf("candidate sha %s is not a commit in worktree: %w", sha, err)
	}
	if err := runGit(ctx, worktreePath, "merge-base", "--is-ancestor", sha, "HEAD"); err != nil {
		return fmt.Errorf("candidate sha %s is not on current branch HEAD (stale integration tip)", sha)
	}
	return nil
}

func (in *Integration) runMergeGate(ctx context.Context, rg ReviewGateOutcome) (*MergeOutcome, error) {
	mo := &MergeOutcome{
		SHA:    rg.SHA,
		Branch: rg.Branch,
	}

	if in.DryRun {
		mo.CherryPicked = true
		return mo, nil
	}

	dl := lock.NewDirLock(in.LockDir)
	if err := dl.Acquire(ctx, in.MaxMergeAge, fmt.Sprintf("merge %s/%s", rg.Branch, rg.SHA)); err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	defer dl.Release()

	if err := in.prepareMain(ctx); err != nil {
		return nil, fmt.Errorf("prepare main: %w", err)
	}

	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		mergeSHA, conflict, err := in.cherryPick(ctx, rg.SHA)
		if err != nil {
			if conflict {
				mo.Conflict = true
				return nil, fmt.Errorf("cherry-pick conflict for %s: %w", rg.SHA, err)
			}
			return nil, fmt.Errorf("cherry-pick attempt %d: %w", attempt+1, err)
		}

		// Empty cherry-pick means the patch is already applied.
		if mergeSHA == "" {
			mo.CherryPicked = true
			mo.AlreadyMerged = true
			head, _ := gitOutput(ctx, in.RepoRoot, "rev-parse", "origin/main")
			mo.MergeSHA = strings.TrimSpace(head)
			return mo, nil
		}

		mo.MergeSHA = mergeSHA

		// Test gate after cherry-pick, before push.
		if in.Verifier != nil {
			vr, vErr := in.Verifier.Execute(ctx, in.RepoRoot)
			if vErr != nil {
				_ = runGit(ctx, in.RepoRoot, "reset", "--hard", "origin/main")
				return nil, fmt.Errorf("verifier error: %w", vErr)
			}
			if vr != nil && !vr.Passed {
				_ = runGit(ctx, in.RepoRoot, "reset", "--hard", "origin/main")
				return nil, fmt.Errorf("verifier failed: %s", vr.Output)
			}
		}

		// Push
		if pushErr := runGit(ctx, in.RepoRoot, "push", "origin", "main"); pushErr == nil {
			mo.CherryPicked = true
			mo.Pushed = true
			return mo, nil
		}

		// Push failed — fetch and check if our commit made it upstream.
		_ = runGit(ctx, in.RepoRoot, "fetch", "-q", "origin", "main")
		if err := runGit(ctx, in.RepoRoot, "merge-base", "--is-ancestor", mergeSHA, "origin/main"); err == nil {
			mo.CherryPicked = true
			mo.AlreadyMerged = true
			return mo, nil
		}

		// Someone else pushed. Reset to origin/main and retry.
		if err := runGit(ctx, in.RepoRoot, "reset", "--hard", "origin/main"); err != nil {
			return nil, fmt.Errorf("reset after push race: %w", err)
		}
	}

	return nil, fmt.Errorf("push failed after %d attempts for %s", maxAttempts, rg.SHA)
}

func (in *Integration) prepareMain(ctx context.Context) error {
	current, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("rev-parse HEAD: %w", err)
	}
	if strings.TrimSpace(current) != "main" {
		if err := runGit(ctx, in.RepoRoot, "checkout", "main"); err != nil {
			return fmt.Errorf("checkout main: %w", err)
		}
	}
	if err := runGit(ctx, in.RepoRoot, "fetch", "-q", "origin", "main"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := runGit(ctx, in.RepoRoot, "merge", "--ff-only", "origin/main"); err != nil {
		return fmt.Errorf("ff-only merge: %w", err)
	}
	return nil
}

func (in *Integration) cherryPick(ctx context.Context, sha string) (mergeSHA string, conflict bool, err error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "cherry-pick", sha)
	cmd.Dir = in.RepoRoot
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		if strings.Contains(stderrStr, "nothing to commit") || strings.Contains(stderrStr, "empty") {
			_ = runGit(ctx, in.RepoRoot, "cherry-pick", "--abort")
			return "", false, nil
		}
		if hasUnmergedPaths(ctx, in.RepoRoot) {
			_ = runGit(ctx, in.RepoRoot, "cherry-pick", "--abort")
			return "", true, fmt.Errorf("conflict: %s", stderrStr)
		}
		return "", false, fmt.Errorf("cherry-pick: %w\n%s", err, stderrStr)
	}

	if hasUnmergedPaths(ctx, in.RepoRoot) {
		_ = runGit(ctx, in.RepoRoot, "cherry-pick", "--abort")
		return "", true, fmt.Errorf("unmerged paths after cherry-pick")
	}

	head, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(head), false, nil
}

func (in *Integration) runCleanup(ctx context.Context, uw UnmergedWork) (bool, error) {
	// Refuse if the worktree is dirty.
	statusOut, err := gitOutput(ctx, uw.WorktreePath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("status: %w", err)
	}
	if strings.TrimSpace(statusOut) != "" {
		return false, fmt.Errorf("worktree %s is dirty, refusing cleanup", uw.WorktreePath)
	}

	// Refuse if the worktree still has unique unmerged commits.
	cherryOut, err := gitOutput(ctx, uw.WorktreePath, "cherry", "origin/main", uw.Branch)
	if err != nil {
		return false, nil
	}
	for _, line := range strings.Split(cherryOut, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+ ") {
			return false, fmt.Errorf("worktree %s has unique unmerged commits, refusing cleanup", uw.WorktreePath)
		}
	}

	// Session teardown.
	if in.SessionManager != nil {
		if err := in.SessionManager.Stop(ctx, uw.Branch); err != nil {
			return false, fmt.Errorf("session stop: %w", err)
		}
	}

	// Remove the worktree.
	if err := runGit(ctx, in.RepoRoot, "worktree", "remove", uw.WorktreePath); err != nil {
		return false, fmt.Errorf("worktree remove: %w", err)
	}

	// Delete the branch.
	_ = runGit(ctx, in.RepoRoot, "branch", "-D", uw.Branch)

	return true, nil
}

// -- helpers --

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, string(out))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(out), nil
}

// hasUnmergedPaths returns true when the working tree has files with
// unresolved merge conflicts. Uses git diff --diff-filter=U which lists
// unmerged paths only — NOT git diff --check (which catches whitespace
// errors, not conflict markers).
func hasUnmergedPaths(ctx context.Context, dir string) bool {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)) != ""
}

// branchToRef attempts to extract a ticket ref from a branch name.
// Branches follow "task/FAC-123-description" or "lane".
func branchToRef(branch string) string {
	if !strings.Contains(branch, "/") {
		return branch
	}
	parts := strings.SplitN(branch, "/", 2)
	if len(parts) == 2 {
		slug := parts[1]
		if idx := strings.Index(slug, "-"); idx > 0 {
			candidate := slug[:idx]
			if len(candidate) > 3 && candidate == strings.ToUpper(candidate) {
				return candidate
			}
			rest := slug[idx+1:]
			splitAt := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
			if splitAt > 0 {
				return candidate + "-" + rest[:splitAt]
			}
		}
	}
	return branch
}
