package harvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/resources"
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
//     checkout main, fetch origin/main, fail-closed serialized Replay,
//     test gate, push after complete ordered batch.
//  4. Post-merge — proof-readback via sync.LandedProof, board-complete
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
	DiskAdmission   resources.DiskAdmission
	readback        func(context.Context, string) error
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

// WithDiskAdmission replaces the default read-only capacity authority.
func WithDiskAdmission(admission resources.DiskAdmission) IntegrationOption {
	return func(i *Integration) { i.DiskAdmission = admission }
}

// IntegrationResult carries the outcome of the full pipeline for one harvest.
type IntegrationResult struct {
	HarvestResult      *HarvestResult      `json:"harvest_result"`
	ReviewGatedSHAs    []ReviewGateOutcome `json:"review_gated_shas,omitempty"`
	MergedSHAs         []MergeOutcome      `json:"merged_shas,omitempty"`
	BoardCompletedRefs []string            `json:"board_completed_refs,omitempty"`
	CleanedWorktrees   []string            `json:"cleaned_worktrees,omitempty"`
	Errors             []string            `json:"errors,omitempty"`
}

// ReviewGateOutcome records the result of the review gate for a SHA.
// Reason is the structured Admit rejection reason (diagnostic only);
// Eligible is the sole merge-authority signal.
type ReviewGateOutcome struct {
	SHA      string `json:"sha"`
	Task     string `json:"task,omitempty"`
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
		Harvester:     h,
		Verifier:      v,
		Dispatcher:    d,
		Ledger:        l,
		RepoRoot:      repoRoot,
		LockDir:       filepath.Join(repoRoot, ".git", "herd-shared-checkout.lock.d"),
		MaxMergeAge:   5 * time.Minute,
		DiskAdmission: resources.NewCapacityGate(resources.OSBackend{}, resources.DefaultDiskPolicy()),
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Run executes the full harvest → review-gate → merge-gate → post-merge → cleanup flow.
func (in *Integration) Run(ctx context.Context) (*IntegrationResult, error) {
	res := &IntegrationResult{}

	// Phase 1: Harvest
	var hr *HarvestResult
	var err error
	if in.DryRun {
		hr, err = in.Harvester.HarvestReadOnly(ctx)
	} else {
		hr, err = in.Harvester.Harvest(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("harvest: %w", err)
	}
	res.HarvestResult = hr
	if len(hr.UnmergedWorktrees) == 0 {
		return res, nil
	}

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

	// Phase 3+4: one serialized replay/verification/publish transaction per
	// worktree. A worktree is never split into singleton publications.
	groups := make([][]ReviewGateOutcome, 0, len(hr.UnmergedWorktrees))
	for _, uw := range hr.UnmergedWorktrees {
		group := make([]ReviewGateOutcome, 0, len(uw.Unmerged))
		for _, rg := range res.ReviewGatedSHAs {
			if rg.Worktree == uw.WorktreePath {
				group = append(group, rg)
			}
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	for _, group := range groups {
		if len(group) == 0 || !allEligible(group) || in.DryRun {
			continue
		}
		mos, err := in.runMergeBatch(ctx, group)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("merge batch %s: %v", group[0].Task, err))
			continue
		}
		res.MergedSHAs = append(res.MergedSHAs, mos...)
		ref := hsync.NormalizeRef(branchToRef(group[0].Branch))
		finalHead := mos[len(mos)-1].MergeSHA
		batchOK := true
		for i := range mos {
			if proofErr := in.mergeReadback(ctx, group[0].Worktree); proofErr != nil {
				batchOK = false
				res.Errors = append(res.Errors, fmt.Sprintf("merge readback %s order=%d: %v", ref, i, proofErr))
			}
		}
		if err := ensureRemoteReplayHead(ctx, in.RepoRoot, finalHead); err != nil {
			batchOK = false
			res.Errors = append(res.Errors, err.Error())
		}
		if !batchOK {
			continue
		}
		if in.Ledger != nil {
			for i, mo := range mos {
				if ledgerErr := in.Ledger.Consumed(group[i].SHA, mo.MergeSHA); ledgerErr != nil {
					batchOK = false
					res.Errors = append(res.Errors, fmt.Sprintf("ledger consumed %s: %v", group[i].SHA, ledgerErr))
				}
			}
		}
		if !batchOK {
			continue
		}
		if in.Dispatcher != nil {
			if boardErr := in.Dispatcher.BoardComplete(ctx, ref, finalHead); boardErr != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("board-complete %s: %v", ref, boardErr))
				continue
			}
			res.BoardCompletedRefs = append(res.BoardCompletedRefs, ref)
		}
		mergedByWorktree[group[0].Worktree] = len(group)
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
	outcome.Task = adm.Task

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
	// Singleton path delegates to the compiled Replay authority via runMergeBatch.
	// Replay is invoked there so ordered batch and singleton share one publish gate.
	mos, err := in.runMergeBatch(ctx, []ReviewGateOutcome{rg})
	if err != nil {
		return nil, err
	}
	if len(mos) != 1 {
		return nil, fmt.Errorf("singleton merge gate received batch cardinality %d", len(mos))
	}
	return &mos[0], nil
}

func allEligible(group []ReviewGateOutcome) bool {
	for _, rg := range group {
		if !rg.Eligible || rg.Task == "" {
			return false
		}
	}
	return true
}

func (in *Integration) mergeReadback(ctx context.Context, worktreeDir string) error {
	if in.readback != nil {
		return in.readback(ctx, worktreeDir)
	}
	return hsync.LandedProof(worktreeDir)
}

func (in *Integration) runMergeBatch(ctx context.Context, group []ReviewGateOutcome) ([]MergeOutcome, error) {
	if len(group) == 0 || in.DryRun {
		return nil, nil
	}
	if err := in.admitMergeDisk(group[0].Worktree); err != nil {
		return nil, err
	}

	dl := lock.NewDirLock(in.LockDir)
	if err := dl.Acquire(ctx, in.MaxMergeAge, fmt.Sprintf("merge batch %s/%s", group[0].Branch, group[0].SHA)); err != nil {
		return nil, fmt.Errorf("lock acquire: %w", err)
	}
	defer dl.Release()

	if err := in.prepareMain(ctx); err != nil {
		return nil, fmt.Errorf("prepare current main: %w", err)
	}
	expected, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	repoID, err := canonicalRepoIdentity(ctx, in.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve portable repository identity: %w", err)
	}
	task := group[0].Task
	sources := make([]string, len(group))
	for i, rg := range group {
		if rg.Task != task {
			return nil, errors.New("replay batch mixes task identities")
		}
		sources[i] = rg.SHA
	}
	replayReq := ReplayRequest{RepoRoot: in.RepoRoot, TaskID: task, RepoID: repoID, ExpectedHead: strings.TrimSpace(expected), Generation: "integration/" + task + "/" + group[0].SHA, SourceCommits: sources}
	replay, err := Replay(ctx, replayReq)
	if err != nil {
		return nil, err
	}
	if replay == nil || !replay.Completed || len(replay.Items) != len(group) {
		return nil, fmt.Errorf("replay did not complete the ordered batch")
	}
	mos := make([]MergeOutcome, len(group))
	for i, item := range replay.Items {
		if item.Source != group[i].SHA || item.Classification == ReplayUnresolved || item.DestinationHead == "" || item.Matched == "" {
			return nil, fmt.Errorf("replay mapping incomplete at order %d", i)
		}
		mos[i] = MergeOutcome{SHA: group[i].SHA, Branch: group[i].Branch, CherryPicked: item.Classification == ReplayAppliedExact, AlreadyMerged: item.Classification != ReplayAppliedExact, MergeSHA: item.DestinationHead}
	}
	if err := ensureReplayHead(ctx, in.RepoRoot, replay.FinalHead); err != nil {
		return nil, err
	}
	if in.Verifier != nil {
		vr, vErr := in.Verifier.Execute(ctx, in.RepoRoot)
		if vErr != nil {
			blocked := RecordReplayBlocked(ctx, replayReq, "verifier error: "+vErr.Error())
			return nil, errors.Join(fmt.Errorf("verifier error: %w", vErr), blocked)
		}
		if vr != nil && !vr.Passed {
			blocked := RecordReplayBlocked(ctx, replayReq, "verifier failed: "+vr.Output)
			return nil, errors.Join(fmt.Errorf("verifier failed: %s", vr.Output), blocked)
		}
	}
	if err := ensureReplayHead(ctx, in.RepoRoot, replay.FinalHead); err != nil {
		return nil, err
	}
	if err := runGit(ctx, in.RepoRoot, "push", "origin", "main"); err != nil {
		return nil, fmt.Errorf("push after complete replay batch: %w", err)
	}
	for i := range mos {
		mos[i].Pushed = true
	}
	return mos, nil
}

func (in *Integration) admitMergeDisk(worktreePath string) error {
	if in == nil || in.DiskAdmission == nil {
		return fmt.Errorf("disk capacity gate unavailable for merge")
	}
	parts := []resources.DiskRequirement{resources.DefaultMergeRequirement()}
	if in.Verifier != nil {
		// Build/test artifacts need independent headroom from the merge itself.
		parts = append(parts, resources.DefaultWorktreeCreateRequirement())
	}
	requirement, err := resources.AggregateDiskRequirement(parts...)
	if err != nil {
		return fmt.Errorf("disk capacity gate: invalid merge requirement")
	}
	repo, err := resources.ResolveExistingPath(in.RepoRoot)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve repository volume")
	}
	worktree, err := resources.ResolveExistingPath(worktreePath)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve worktree volume")
	}
	tmp, err := resources.ResolveExistingPath(os.TempDir())
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve temporary volume")
	}
	decision := in.DiskAdmission.Admit(resources.DiskRequest{
		Operation: "merge_gate", Path: repo, TempPath: tmp,
		RequiredBytes: requirement.Bytes, RequiredInodes: requirement.Inodes,
		AdditionalPaths: []string{worktree},
	})
	if decision.Allowed {
		return nil
	}
	evidence, _ := json.Marshal(decision.Evidence)
	return fmt.Errorf("disk capacity gate blocked: state=%s evidence=%s", decision.State, evidence)
}

func (in *Integration) prepareMain(ctx context.Context) error {
	current, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("rev-parse HEAD: %w", err)
	}
	if strings.TrimSpace(current) != "main" {
		// Fail closed for the serialized replay authority: never silently
		// switch checkouts under a completed mapping. Operators must land
		// on main before publishing a batch.
		return fmt.Errorf("integration checkout is %q, want main", strings.TrimSpace(current))
	}
	if err := runGit(ctx, in.RepoRoot, "fetch", "-q", "origin", "main"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	local, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remote, err := gitOutput(ctx, in.RepoRoot, "rev-parse", "origin/main")
	if err != nil {
		return err
	}
	if strings.TrimSpace(local) != strings.TrimSpace(remote) {
		if err := runGit(ctx, in.RepoRoot, "merge", "--ff-only", "origin/main"); err != nil {
			return fmt.Errorf("stale destination head cannot be fast-forwarded safely: local=%s origin/main=%s: %w", strings.TrimSpace(local), strings.TrimSpace(remote), err)
		}
	}
	return nil
}

func ensureReplayHead(ctx context.Context, repo, want string) error {
	head, err := gitOutput(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != strings.TrimSpace(want) {
		return fmt.Errorf("destination head changed after complete replay: got=%s want=%s", strings.TrimSpace(head), strings.TrimSpace(want))
	}
	return nil
}

func ensureRemoteReplayHead(ctx context.Context, repo, want string) error {
	if err := runGit(ctx, repo, "fetch", "-q", "origin", "main"); err != nil {
		return fmt.Errorf("remote readback fetch: %w", err)
	}
	head, err := gitOutput(ctx, repo, "rev-parse", "origin/main")
	if err != nil {
		return fmt.Errorf("remote readback head: %w", err)
	}
	if strings.TrimSpace(head) != strings.TrimSpace(want) {
		return fmt.Errorf("remote readback head mismatch: got=%s want=%s", strings.TrimSpace(head), strings.TrimSpace(want))
	}
	return nil
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
	cmd := execCommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, string(out))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := execCommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(out), nil
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
