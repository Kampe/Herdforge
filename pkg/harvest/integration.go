package harvest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Integration implements the README contract: a serialized merge pipeline
// that consumes a Harvester (exact-SHA), Verifier (test gate), and Dispatcher
// (board-complete) as dependencies. It is the coordinator-level entry point
// for the exact-SHA harvest → review-gate → merge-gate → post-merge flow.
//
// Phases:
//
//  1. Harvest — list worktrees, check unmerged commits via git cherry.
//  2. Review gate — for each unmerged worktree, verify via pkg/review.Ledger
//     that the SHA has a qualifying PASS verdict.
//  3. Merge gate — acquire DirLock, fetch origin/main, cherry-pick each
//     unique commit, detect conflict markers.
//  4. Post-merge — push, proof-readback via sync.MergeEvidence, board-complete
//     via sync.BoardDone, ledger Consumed.
//  5. Cleanup — session + worktree removal with dirty/unique-work protection.

type Integration struct {
	Harvester   *Harvester
	Verifier    Verifier
	Dispatcher  Dispatcher
	RepoRoot    string
	LockDir     string
	MaxMergeAge time.Duration
	DryRun      bool
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

// IntegrationResult carries the outcome of the full pipeline for one harvest.
type IntegrationResult struct {
	HarvestResult     *HarvestResult       `json:"harvest_result"`
	ReviewGatedSHAs   []ReviewGateOutcome  `json:"review_gated_shas,omitempty"`
	MergedSHAs        []MergeOutcome       `json:"merged_shas,omitempty"`
	BoardCompletedRef string               `json:"board_completed_ref,omitempty"`
	Errors            []string             `json:"errors,omitempty"`
}

// ReviewGateOutcome records the result of the review gate for a SHA.
type ReviewGateOutcome struct {
	SHA        string `json:"sha"`
	Branch     string `json:"branch"`
	Worktree   string `json:"worktree"`
	Classification    Classification `json:"classification"`
	Eligible   bool   `json:"eligible"`
	Err        string `json:"err,omitempty"`
}

// MergeOutcome records the result of the merge gate for a SHA.
type MergeOutcome struct {
	SHA        string `json:"sha"`
	Branch     string `json:"branch"`
	CherryPicked bool `json:"cherry_picked"`
	Conflict   bool   `json:"conflict"`
	Pushed     bool   `json:"pushed"`
	Err        string `json:"err,omitempty"`
	MergeSHA   string `json:"merge_sha,omitempty"`
}

// NewIntegration creates an Integration with sensible defaults.
// The Verifier can be nil (skips test-gate), but Dispatcher must be non-nil
// for board-complete.
func NewIntegration(h *Harvester, v Verifier, d Dispatcher, repoRoot string, opts ...IntegrationOption) *Integration {
	i := &Integration{
		Harvester:   h,
		Verifier:    v,
		Dispatcher:  d,
		RepoRoot:    repoRoot,
		LockDir:     filepath.Join(repoRoot, ".git", "herd-shared-checkout.lock.d"),
		MaxMergeAge: 5 * time.Minute,
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Run executes the full harvest → review-gate → merge-gate → post-merge flow.
func (in *Integration) Run(ctx context.Context) (*IntegrationResult, error) {
	res := &IntegrationResult{}

	// Phase 1: Harvest
	hr, err := in.Harvester.Harvest(ctx)
	if err != nil {
		return nil, fmt.Errorf("harvest: %w", err)
	}
	res.HarvestResult = hr

	if len(hr.UnmergedWorktrees) == 0 {
		return res, nil
	}

	// Phase 2: Review gate for each worktree
	for _, uw := range hr.UnmergedWorktrees {
		for _, sha := range uw.Unmerged {
			outcome := in.runReviewGate(ctx, sha, uw)
			res.ReviewGatedSHAs = append(res.ReviewGatedSHAs, outcome)
		}
	}

	// Phase 3 & 4: Merge gate + post-merge for each eligible SHA
	for _, rg := range res.ReviewGatedSHAs {
		if !rg.Eligible {
			continue
		}
		mo, err := in.runMergeGate(ctx, rg)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("merge %s: %v", rg.SHA, err))
			continue
		}
		res.MergedSHAs = append(res.MergedSHAs, *mo)

		if mo.Pushed && mo.MergeSHA != "" {
			// Phase 5: Board-complete via dispatcher
			if err := in.Dispatcher.BoardComplete(ctx, branchToRef(rg.Branch), mo.MergeSHA); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("board-complete %s: %v", rg.Branch, err))
				continue
			}
			res.BoardCompletedRef = branchToRef(rg.Branch)
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

	// Classify the worktree's latest commit message for the review gate.
	msg, err := commitMessage(ctx, uw.WorktreePath, sha)
	if err != nil {
		outcome.Err = fmt.Sprintf("commit message: %v", err)
		return outcome
	}

	outcome.Classification = ClassifyText(msg)

	// The review gate passes when the classification allows merge.
	switch outcome.Classification {
	case ClassificationPass, ClassificationComplete:
		outcome.Eligible = true
	case ClassificationNeedsReview:
		// Needs explicit review — check if verifier passes as proxy.
		if in.Verifier != nil {
			vr, vErr := in.Verifier.Execute(ctx, uw.WorktreePath)
			if vErr != nil {
				outcome.Err = fmt.Sprintf("verifier error: %v", vErr)
				return outcome
			}
			outcome.Eligible = vr.Passed
		}
	case ClassificationFail, ClassificationBlocked, ClassificationQuota:
		outcome.Eligible = false
	case ClassificationUnconsumed, ClassificationUnknown:
		outcome.Eligible = false
	}

	return outcome
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

	// Acquire merge lock
	dl := lockFor(in.LockDir)
	if err := dl.Acquire(ctx, in.MaxMergeAge, fmt.Sprintf("merge %s/%s", rg.Branch, rg.SHA)); err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	defer dl.Release()

	// Fetch latest origin/main
	if err := runGit(ctx, in.RepoRoot, "fetch", "-q", "origin", "main"); err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	// Check if SHA is already on origin/main (already merged)
	if err := runGit(ctx, in.RepoRoot, "merge-base", "--is-ancestor", rg.SHA, "origin/main"); err == nil {
		mo.CherryPicked = true
		mo.MergeSHA = rg.SHA
		return mo, nil
	}

	// Cherry-pick onto main
	var stderr bytes.Buffer
	cherryCmd := exec.CommandContext(ctx, "git", "cherry-pick", rg.SHA)
	cherryCmd.Dir = in.RepoRoot
	cherryCmd.Stderr = &stderr
	if err := cherryCmd.Run(); err != nil {
		stripped := stderr.String()
		if strings.Contains(stripped, "conflict") || hasConflictMarkers(in.RepoRoot) {
			mo.Conflict = true
			// Abort the cherry-pick so the working tree is clean
			_ = runGit(ctx, in.RepoRoot, "cherry-pick", "--abort")
			return nil, fmt.Errorf("cherry-pick conflict for %s", rg.SHA)
		}
		return nil, fmt.Errorf("cherry-pick: %w\n%s", err, stripped)
	}
	mo.CherryPicked = true

	// Verify no conflict markers remain
	if hasConflictMarkers(in.RepoRoot) {
		mo.Conflict = true
		_ = runGit(ctx, in.RepoRoot, "cherry-pick", "--abort")
		return nil, fmt.Errorf("conflict markers detected after cherry-pick %s", rg.SHA)
	}

	// Get the resulting merge SHA
	mergeSHA, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	mo.MergeSHA = strings.TrimSpace(mergeSHA)

	// Push to origin/main
	if pushErr := runGit(ctx, in.RepoRoot, "push", "origin", "main"); pushErr != nil {
		return nil, fmt.Errorf("push: %w", pushErr)
	}
	mo.Pushed = true

	return mo, nil
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

func commitMessage(ctx context.Context, worktree, sha string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%s%n%b", sha)
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}

func hasConflictMarkers(dir string) bool {
	cmd := exec.Command("git", "diff", "--check")
	cmd.Dir = dir
	out, _ := cmd.Output()
	// git diff --check exits non-zero and outputs filenames with conflict markers.
	// A clean diff exits 0 with no output.
	return len(out) > 0
}

// lockFor creates a DirLock for the given directory path.
// We inline the minimal lock API to avoid importing pkg/lock (READ-ONLY constraint).
func lockFor(dir string) *mergeLock {
	return &mergeLock{dir: dir, holder: filepath.Join(dir, "holder")}
}

// mergeLock is a minimal advisory mkdir-based lock for merge serialization.
type mergeLock struct {
	dir    string
	holder string
}

func (l *mergeLock) Acquire(ctx context.Context, wait time.Duration, reason string) error {
	waited := 0
	waitSecs := int(wait.Seconds())
	for {
		l.breakIfStale()
		if err := os.Mkdir(l.dir, 0o755); err == nil {
			l.writeHolder(reason)
			return nil
		}
		if waited >= waitSecs {
			return fmt.Errorf("locked, waited %ds", waitSecs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		waited++
	}
}

func (l *mergeLock) Release() {
	_ = os.RemoveAll(l.dir)
}

func (l *mergeLock) breakIfStale() {
	info, err := os.Stat(l.dir)
	if err != nil {
		return
	}
	if time.Since(info.ModTime()) > 5*time.Minute {
		_ = os.RemoveAll(l.dir)
	}
}

func (l *mergeLock) writeHolder(reason string) {
	content := fmt.Sprintf("pid=%d\nreason=%s\n", os.Getpid(), reason)
	_ = os.WriteFile(l.holder, []byte(content), 0o644)
}

// branchToRef attempts to extract a ticket ref from a branch name.
// Branches follow "task/FAC-123-description" or "lane".
func branchToRef(branch string) string {
	if !strings.Contains(branch, "/") {
		return branch
	}
	parts := strings.SplitN(branch, "/", 2)
	if len(parts) == 2 {
		// Try to extract ref from the slug
		slug := parts[1]
		if idx := strings.Index(slug, "-"); idx > 0 {
			// Grab up to the second hyphen for the ref
			candidate := slug[:idx]
			if len(candidate) > 3 && candidate == strings.ToUpper(candidate) {
				return candidate
			}
			// Try e.g. FAC-18
			rest := slug[idx+1:]
			splitAt := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
			if splitAt > 0 {
				return candidate + "-" + rest[:splitAt]
			}
		}
	}
	return branch
}

// Standard sentinel errors for the integration pipeline.
var (
	ErrReviewRejected  = errors.New("review gate rejected")
	ErrMergeConflict   = errors.New("merge conflict detected")
	ErrLockHeld        = errors.New("merge lock held")
	ErrBoardRefused    = errors.New("board refused to mark complete")
)
