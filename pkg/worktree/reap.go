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

// LeaseGenerationProbe returns the current lease/session generation for an
// exact worktree. A missing generation is not evidence that removal is safe.
// The generation is carried into the action fence and checked immediately
// before removal.
type LeaseGenerationProbe func(ctx context.Context, path, branch string) (generation string, err error)

// BoardEvidenceProbe reads the current integration/action evidence for an
// exact target from the board or integration provider.
type BoardEvidenceProbe func(ctx context.Context, path, branch string) (evidence string, err error)

// ReapReceiptSink durably records one portable per-target outcome. The sink
// receives no filesystem path, so its output can be moved between worktrees.
type ReapReceiptSink func(ReapReceipt) error

// ReapEvidence is the immutable evidence bundle required for destructive
// reap. These values are deliberately opaque to this package: the integration
// and board providers own their meaning, while GC binds them to every target.
type ReapEvidence struct {
	IntegrationSHA  string
	BoardEvidence   string
	LeaseGeneration string
	PolicyDigest    string
	Actor           string
}

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
	// LeaseGenerationProbe is required for destructive action. LeaseProbe
	// answers active/inactive; this probe supplies the generation fence.
	LeaseGenerationProbe LeaseGenerationProbe
	BoardEvidenceProbe   BoardEvidenceProbe
	ReceiptSink          ReapReceiptSink
	// Evidence is required for destructive action and is copied into each
	// candidate's action binding.
	Evidence ReapEvidence
	// ActionPolicy is the one explicit policy decision. The zero value is
	// report-only; destructive action requires exactly "remove".
	ActionPolicy string
}

// ReapCandidate is one classified worktree with evidence and preservation guidance.
type ReapCandidate struct {
	Path             string
	Branch           string
	HEAD             string
	Class            ReapClass
	UniqueSHAs       []string
	Reason           string
	PreserveAction   string
	SalvageRef       string
	SalvageOK        bool
	Integration      string // resolved integration base used for git cherry
	IntegrationSHA   string
	LeaseGeneration  string
	PolicyDigest     string
	BoardEvidence    string
	ActionPolicy     string
	Actor            string
	EvidenceObserved bool
	// Eligible is true only when Class is content-merged; salvage is created and
	// verified immediately before destructive removal, never during planning.
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
	// Receipts are portable outcomes. Target is a branch or stable role label,
	// never an absolute filesystem path.
	Receipts []ReapReceipt
}

type ReapReceipt struct {
	Target           string `json:"target"`
	Branch           string `json:"branch"`
	HEAD             string `json:"head"`
	Outcome          string `json:"outcome"`
	ReasonCode       string `json:"reason_code"`
	Reason           string `json:"reason"`
	IntegrationSHA   string `json:"integration_sha"`
	LeaseGeneration  string `json:"lease_generation"`
	PolicyDigest     string `json:"policy_digest"`
	BoardEvidence    string `json:"board_evidence"`
	ActionPolicy     string `json:"action_policy"`
	Actor            string `json:"actor"`
	EvidenceObserved bool   `json:"evidence_observed"`
	SalvageRef       string `json:"salvage_ref"`
}

func reapReceipt(c ReapCandidate, outcome, _ string) ReapReceipt {
	target := c.Branch
	if target == "" {
		target = "root"
	}
	return ReapReceipt{
		Target: target, Branch: c.Branch, HEAD: c.HEAD, Outcome: outcome,
		ReasonCode: reapReasonCode(c, outcome), Reason: "reap outcome: " + reapReasonCode(c, outcome),
		IntegrationSHA: c.IntegrationSHA, LeaseGeneration: c.LeaseGeneration,
		PolicyDigest: c.PolicyDigest, BoardEvidence: c.BoardEvidence,
		ActionPolicy: c.ActionPolicy, Actor: c.Actor, EvidenceObserved: c.EvidenceObserved,
		SalvageRef: c.SalvageRef,
	}
}

func reapReasonCode(c ReapCandidate, outcome string) string {
	switch outcome {
	case "remove-intent":
		return "removal-intent"
	case "removed":
		return "removed-verified"
	case "unverified":
		return "removal-unverified"
	case "already-absent":
		return "target-already-absent"
	case "refused":
		if c.Class == ReapClassRoot {
			return "protected-root"
		}
		if c.Class == ReapClassDirty {
			return "dirty-worktree"
		}
		if c.Class == ReapClassUnique {
			return "unique-commits"
		}
		if c.Class == ReapClassProtected {
			return "protected-worktree"
		}
		return "unknown-evidence"
	default:
		if c.Eligible {
			return "eligible"
		}
		return "unknown-outcome"
	}
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
	if policy.AutoReap {
		if err := validateDestructivePolicy(policy); err != nil {
			return nil, err
		}
	}
	report, err := w.PlanReap(ctx, policy)
	if err != nil {
		return nil, err
	}
	if !policy.AutoReap {
		for _, candidate := range report.Candidates {
			outcome := "refused"
			if candidate.Eligible {
				outcome = "planned"
			}
			report.Receipts = append(report.Receipts, reapReceipt(candidate, outcome, candidate.Reason))
		}
		return report, nil
	}
	// Initial refusals are durable action outcomes and must be persisted before
	// the first eligible target can reach intent, salvage, or removal.
	for _, refused := range report.Refused {
		if err := w.recordReceipt(report, policy, reapReceipt(refused, "refused", refused.Reason)); err != nil {
			return report, err
		}
	}
	seen := make(map[string]bool, len(report.Candidates))
	for _, candidate := range report.Candidates {
		seen[normalizePath(candidate.Path)] = true
	}
	for _, target := range policy.TargetPaths {
		if !seen[normalizePath(target)] {
			return report, fmt.Errorf("reap action refused: exact target is not a registered worktree")
		}
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
			absent := planned
			absent.Class = ReapClassUnknown
			absent.Eligible = false
			absent.Reason = "target disappeared before JIT action"
			absent.PreserveAction = "record already-absent outcome; do not retry blindly"
			report.Refused = append(report.Refused, absent)
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(absent, "already-absent", absent.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}
		integration, ierr := w.resolveIntegrationBase(ctx, policy.DefaultBranch)
		reclass := w.classifyOne(ctx, current, policy, integration, ierr)
		if !sameReapBinding(planned, reclass) {
			reclass.Class = ReapClassUnknown
			reclass.Eligible = false
			reclass.Reason = "action binding changed since plan"
			reclass.PreserveAction = "keep worktree; re-plan with current evidence"
		}
		if !reclass.Eligible {
			report.Refused = append(report.Refused, reclass)
			if err := w.recordReceipt(report, policy, reapReceipt(reclass, "refused", reclass.Reason)); err != nil {
				return report, err
			}
			report.Errors = append(report.Errors,
				fmt.Sprintf("%s: JIT revalidation refused reap: %s (%s)", reclass.Path, reclass.Class, reclass.Reason))
			continue
		}
		if policy.LeaseGenerationProbe != nil {
			generation, gerr := policy.LeaseGenerationProbe(ctx, reclass.Path, reclass.Branch)
			if gerr != nil || generation == "" || generation != policy.Evidence.LeaseGeneration {
				reclass.Class = ReapClassUnknown
				reclass.Eligible = false
				reclass.Reason = "lease generation changed or unavailable"
				reclass.PreserveAction = "keep worktree until the lease fence is revalidated"
				report.Refused = append(report.Refused, reclass)
				if sinkErr := w.recordReceipt(report, policy, reapReceipt(reclass, "refused", reclass.Reason)); sinkErr != nil {
					return report, sinkErr
				}
				continue
			}
		}

		// Persist the exact action binding before salvage-ref creation or any
		// removal mutation. A sink failure here is fail-closed by construction.
		if err := w.recordReceipt(report, policy, reapReceipt(reclass, "remove-intent", "validated removal intent")); err != nil {
			return report, err
		}

		// Ensure durable salvage ref points at the tip and verifies before remove.
		if err := w.ensureSalvageRef(ctx, reclass.SalvageRef, reclass.HEAD); err != nil {
			refused := reclass
			refused.Class = ReapClassUnknown
			refused.Eligible = false
			refused.Reason = err.Error()
			refused.PreserveAction = "keep worktree; salvage ref verification failed"
			report.Refused = append(report.Refused, refused)
			report.Errors = append(report.Errors, fmt.Sprintf("%s: salvage: %v", reclass.Path, err))
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(refused, "refused", "salvage ref verification failed")); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}

		// Final fence: the intent sink is a deterministic late-race hook. Re-read
		// every bound immediately after salvage persistence and before removal.
		finalList, finalListErr := w.ListWorktrees(ctx)
		var finalCurrent *WorktreeInfo
		if finalListErr == nil {
			for _, wt := range finalList {
				if wt != nil && sameWorktreePath(wt.Path, planned.Path) {
					finalCurrent = wt
					break
				}
			}
		}
		if finalListErr != nil || finalCurrent == nil {
			absent := planned
			absent.Class = ReapClassUnknown
			absent.Eligible = false
			absent.Reason = "target disappeared before final action fence"
			absent.PreserveAction = "record already-absent outcome; do not retry blindly"
			report.Refused = append(report.Refused, absent)
			if finalListErr != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: final list: %v", planned.Path, finalListErr))
			}
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(absent, "already-absent", absent.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}
		finalIntegration, finalIntegrationErr := w.resolveIntegrationBase(ctx, policy.DefaultBranch)
		final := w.classifyOne(ctx, finalCurrent, policy, finalIntegration, finalIntegrationErr)
		if !final.Eligible || !sameReapBinding(planned, final) {
			finalLeaseBlocked := final.Class == ReapClassProtected && strings.Contains(final.Reason, "active lease")
			final.Class = ReapClassUnknown
			final.Eligible = false
			if sameReapBinding(planned, final) || finalLeaseBlocked {
				final.Reason = "final action fence refused removal"
			} else {
				final.Reason = "final action binding changed after intent"
			}
			final.PreserveAction = "keep worktree; final action fence refused removal"
			report.Refused = append(report.Refused, final)
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(final, "refused", final.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}
		if w.BeforeRemoveFunc != nil {
			if hookErr := w.BeforeRemoveFunc(ctx, reclass.Path); hookErr != nil {
				late := reclass
				late.Class = ReapClassUnknown
				late.Eligible = false
				late.Reason = "removal boundary hook refused action"
				late.PreserveAction = "keep worktree; late mutation was detected"
				report.Refused = append(report.Refused, late)
				if sinkErr := w.recordReceipt(report, policy, reapReceipt(late, "refused", late.Reason)); sinkErr != nil {
					return report, sinkErr
				}
				continue
			}
		}
		if err := w.verifyBoundWorktree(ctx, reclass.Path, reclass.Branch, reclass.HEAD); err != nil {
			late := reclass
			late.Class = ReapClassUnknown
			late.Eligible = false
			late.Reason = "bound HEAD changed immediately before removal"
			late.PreserveAction = "keep worktree; late HEAD drift was detected"
			report.Refused = append(report.Refused, late)
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(late, "refused", late.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}

		if err := w.RemoveWorktreeSafely(ctx, reclass.Path); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: remove: %v", reclass.Path, err))
			failed := reclass
			failed.Class = ReapClassUnknown
			failed.Eligible = false
			failed.Reason = "removal failed"
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(failed, "refused", "removal failed")); sinkErr != nil {
				return report, sinkErr
			}
			continue
		}
		postList, postListErr := w.ListWorktrees(ctx)
		stillRegistered := false
		if postListErr == nil {
			for _, wt := range postList {
				if wt != nil && sameWorktreePath(wt.Path, reclass.Path) {
					stillRegistered = true
					break
				}
			}
		}
		if postListErr != nil || stillRegistered {
			unverified := reclass
			unverified.Class = ReapClassUnknown
			unverified.Eligible = false
			unverified.Reason = "removal outcome unverified"
			unverified.PreserveAction = "keep evidence; removal must be independently verified"
			report.Refused = append(report.Refused, unverified)
			if postListErr != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: post-remove list: %v", reclass.Path, postListErr))
			}
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(unverified, "unverified", unverified.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			return report, fmt.Errorf("reap: removal outcome unverified for target")
		}
		boundRef, boundRefErr := w.revParse(ctx, reclass.Branch)
		if boundRefErr != nil || boundRef != reclass.HEAD {
			unverified := reclass
			unverified.Class = ReapClassUnknown
			unverified.Eligible = false
			unverified.Reason = "branch HEAD changed after removal"
			unverified.PreserveAction = "recover from salvage ref; removal was not verified"
			report.Refused = append(report.Refused, unverified)
			if sinkErr := w.recordReceipt(report, policy, reapReceipt(unverified, "unverified", unverified.Reason)); sinkErr != nil {
				return report, sinkErr
			}
			return report, fmt.Errorf("reap: branch HEAD unverified after removal")
		}
		report.Reaped = append(report.Reaped, reclass.Path)
		if err := w.recordReceipt(report, policy, reapReceipt(reclass, "removed", "explicit removal completed")); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (w *WorktreeManager) verifyBoundWorktree(ctx context.Context, path, branch, head string) error {
	gotHead, err := w.headAt(ctx, path)
	if err != nil || gotHead != head {
		return fmt.Errorf("worktree HEAD drift: got %s want %s", gotHead, head)
	}
	gotBranch, err := w.currentBranchAt(ctx, path)
	if err != nil || gotBranch != branch {
		return fmt.Errorf("worktree branch drift: got %s want %s", gotBranch, branch)
	}
	return nil
}

func (w *WorktreeManager) recordReceipt(report *ReapReport, policy ReapPolicy, receipt ReapReceipt) error {
	if policy.ReceiptSink == nil {
		return fmt.Errorf("reap receipt sink: not configured")
	}
	if err := policy.ReceiptSink(receipt); err != nil {
		return fmt.Errorf("reap receipt sink: %w", err)
	}
	report.Receipts = append(report.Receipts, receipt)
	return nil
}

func validateDestructivePolicy(policy ReapPolicy) error {
	if len(policy.TargetPaths) == 0 {
		return fmt.Errorf("reap action refused: exact TargetPaths are required")
	}
	if policy.LeaseProbe == nil || policy.LeaseGenerationProbe == nil || policy.BoardEvidenceProbe == nil || policy.ReceiptSink == nil {
		return fmt.Errorf("reap action refused: lease/session probes are required")
	}
	if strings.TrimSpace(policy.Evidence.IntegrationSHA) == "" ||
		strings.TrimSpace(policy.Evidence.BoardEvidence) == "" ||
		strings.TrimSpace(policy.Evidence.LeaseGeneration) == "" ||
		strings.TrimSpace(policy.Evidence.PolicyDigest) == "" ||
		strings.TrimSpace(policy.Evidence.Actor) == "" {
		return fmt.Errorf("reap action refused: integration, board, lease, and policy evidence are required")
	}
	if strings.TrimSpace(policy.ActionPolicy) != "remove" {
		return fmt.Errorf("reap action refused: explicit remove action policy is required")
	}
	for name, value := range map[string]string{
		"integration SHA":  policy.Evidence.IntegrationSHA,
		"board evidence":   policy.Evidence.BoardEvidence,
		"lease generation": policy.Evidence.LeaseGeneration,
		"policy digest":    policy.Evidence.PolicyDigest,
		"actor":            policy.Evidence.Actor,
	} {
		if err := validatePortableField(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validatePortableField(name, value string) error {
	if strings.ContainsAny(value, "\x00\r\n") || filepath.IsAbs(value) {
		return fmt.Errorf("reap action refused: %s is not portable", name)
	}
	return nil
}

func sameReapBinding(a, b ReapCandidate) bool {
	return sameWorktreePath(a.Path, b.Path) && a.Branch == b.Branch &&
		a.HEAD == b.HEAD && a.IntegrationSHA == b.IntegrationSHA &&
		a.LeaseGeneration == b.LeaseGeneration &&
		a.PolicyDigest == b.PolicyDigest && a.BoardEvidence == b.BoardEvidence &&
		a.ActionPolicy == b.ActionPolicy && a.Actor == b.Actor &&
		a.EvidenceObserved == b.EvidenceObserved && a.SalvageRef == b.SalvageRef
}

// PruneMergedWorktrees is the historical auto-reap entry point used by pkg/gc.
// It is retained only as a fail-closed compatibility boundary.
func (w *WorktreeManager) PruneMergedWorktrees(ctx context.Context, defaultBranch string) (int, error) {
	// Historical callers have no exact target, lease/session fence, board
	// evidence, or explicit action policy. Keep this compatibility wrapper
	// report-only; destructive callers must use Reap with a fully populated
	// ReapPolicy.
	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	return 0, fmt.Errorf("worktree auto-reap disabled: use Reap with exact targets and action evidence")
}

func (w *WorktreeManager) classifyOne(
	ctx context.Context,
	wt *WorktreeInfo,
	policy ReapPolicy,
	integration string,
	integrationErr error,
) ReapCandidate {
	c := ReapCandidate{
		Path:         wt.Path,
		Branch:       wt.Branch,
		HEAD:         wt.Commit,
		Integration:  integration,
		PolicyDigest: policy.Evidence.PolicyDigest,
		ActionPolicy: policy.ActionPolicy,
		Actor:        policy.Evidence.Actor,
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
	if policy.LeaseGenerationProbe != nil {
		generation, err := policy.LeaseGenerationProbe(ctx, wt.Path, branch)
		if err != nil || strings.TrimSpace(generation) == "" {
			c.Class = ReapClassUnknown
			c.Reason = "lease generation unavailable"
			c.PreserveAction = "keep worktree until lease generation is known"
			return c
		}
		c.LeaseGeneration = generation
		if err := validatePortableField("observed lease generation", generation); err != nil {
			c.LeaseGeneration = ""
			c.Class = ReapClassUnknown
			c.Reason = "lease generation is not portable"
			c.PreserveAction = "keep worktree until lease evidence is portable"
			return c
		}
		if policy.Evidence.LeaseGeneration != "" && generation != policy.Evidence.LeaseGeneration {
			c.Class = ReapClassUnknown
			c.Reason = "lease generation does not match action evidence"
			c.PreserveAction = "keep worktree until the action is replanned"
			return c
		}
	}
	if policy.BoardEvidenceProbe != nil {
		evidence, err := policy.BoardEvidenceProbe(ctx, wt.Path, branch)
		if err != nil || strings.TrimSpace(evidence) == "" {
			c.Class = ReapClassUnknown
			c.Reason = "board/action evidence unavailable"
			c.PreserveAction = "keep worktree until board evidence is readable"
			return c
		}
		c.BoardEvidence = evidence
		if err := validatePortableField("observed board evidence", evidence); err != nil {
			c.BoardEvidence = ""
			c.Class = ReapClassUnknown
			c.Reason = "board evidence is not portable"
			c.PreserveAction = "keep worktree until board evidence is portable"
			return c
		}
		if policy.Evidence.BoardEvidence != "" && evidence != policy.Evidence.BoardEvidence {
			c.Class = ReapClassUnknown
			c.Reason = "board/action evidence does not match action evidence"
			c.PreserveAction = "keep worktree until the action is replanned"
			return c
		}
	}
	// Integration evidence is observed only after the lease/session and board
	// probes succeed. Early refusals intentionally keep observed fields empty;
	// ReapEvidence remains the separate requested/policy context.
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
	actualIntegrationSHA, shaErr := w.revParse(ctx, integration)
	if shaErr != nil {
		c.Class = ReapClassUnknown
		c.Reason = "integration SHA could not be independently resolved"
		c.PreserveAction = "keep worktree until integration ref can be read"
		return c
	}
	c.IntegrationSHA = actualIntegrationSHA
	if err := validatePortableField("observed integration SHA", actualIntegrationSHA); err != nil {
		c.IntegrationSHA = ""
		c.Class = ReapClassUnknown
		c.Reason = "integration evidence is not portable"
		c.PreserveAction = "keep worktree until integration evidence is portable"
		return c
	}
	if policy.Evidence.IntegrationSHA != "" && actualIntegrationSHA != policy.Evidence.IntegrationSHA {
		c.Class = ReapClassUnknown
		c.Reason = "integration SHA does not match action evidence"
		c.PreserveAction = "keep worktree until integration evidence is refreshed"
		return c
	}
	c.EvidenceObserved = true

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
	// Planning is report-only: it neither reads nor writes salvage refs. The
	// action path creates and verifies salvage immediately before removal.
	c.Eligible = true
	return c
}

// SalvageRefFor returns the durable salvage ref for a branch name.
func SalvageRefFor(branch string) string {
	// Valid Git refnames are already portable path components. Preserve their
	// exact spelling: lowercasing would alias herd/FAC-1 and herd/fac-1.
	b := strings.TrimPrefix(branch, "refs/heads/")
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
	if got, err := w.revParse(ctx, ref); err == nil {
		if got != sha {
			return fmt.Errorf("salvage ref %s already protects a different HEAD", ref)
		}
		return nil
	}
	// Create only when absent. The all-zero old value makes this conditional;
	// a concurrent creator cannot be overwritten by a stale reap action.
	zero := strings.Repeat("0", 40)
	cmd := execCommandContext(ctx, "git", "update-ref", ref, sha, zero)
	cmd.Dir = w.RepoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create salvage ref %s: %v (%s)", ref, err, strings.TrimSpace(string(out)))
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
