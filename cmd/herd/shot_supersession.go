package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/committime"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
)

// shotSupersessionFacts is a complete, pre-mutation snapshot. Values with two
// names come from independent authority surfaces and must agree; booleans are
// results of fail-closed cryptographic, lease, or Git checks, never defaults.
type shotSupersessionFacts struct {
	ReportedRef, ReplacementSHA                      string
	ReportedLease                                    int64
	ProviderType, ConfiguredProviderType             string
	ProjectID, ConfiguredProjectID                   string
	TaskRef, TaskID, ProviderTaskRef, ProviderTaskID string
	ProviderTaskProjectID                            string
	ProviderStatus                                   string
	ReceiptVerified, CanonicalReceiptMatches         bool
	Role                                             string
	AuthorityScope                                   string
	LeaseGeneration                                  int64
	LeaseLive                                        bool
	LeaseTaskRef, AuthorizedCandidateSHA             string
	SessionID, LaunchSession                         string
	BuilderSession                                   string
	Model, LaunchModel                               string
	Family, LaunchFamily                             string
	Branch, GitBranch                                string
	Worktree, RegisteredWorktree                     string
	BaseSHA, GitBaseSHA                              string
	GitHeadSHA                                       string
	Clean, ReplacementReachable, LiveLaunch          bool
}

var runShotCandidateSupersession = supersedeShotLifecycleCandidate

func validateShotSupersessionFacts(f shotSupersessionFacts) error {
	equalFold := func(a, b string) bool {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	if !equalFold(f.ReportedRef, f.TaskRef) || !equalFold(f.TaskRef, f.ProviderTaskRef) ||
		strings.TrimSpace(f.TaskID) == "" || f.TaskID != f.ProviderTaskID {
		return fmt.Errorf("candidate supersession: signed task ID/ref does not match provider readback")
	}
	if !equalFold(f.ProviderType, f.ConfiguredProviderType) || f.ProjectID == "" || f.ProjectID != f.ConfiguredProjectID {
		return fmt.Errorf("candidate supersession: signed provider/project does not match configured project")
	}
	if f.ProviderTaskProjectID == "" || f.ProviderTaskProjectID != f.ProjectID {
		return fmt.Errorf("candidate supersession: provider task project does not match signed task context")
	}
	status := provider.NormalizeStatus(f.ProviderStatus)
	if status == provider.StatusUnknown || strings.HasPrefix(status, "unknown:") {
		return fmt.Errorf("candidate supersession: provider status is UNKNOWN")
	}
	if !f.ReceiptVerified || !f.CanonicalReceiptMatches {
		return fmt.Errorf("candidate supersession: signed task context authentication/readback failed")
	}
	role := strings.ToLower(strings.TrimSpace(f.Role))
	switch role {
	case dispatch.RoleWorker, dispatch.RoleRecovery:
	default:
		return fmt.Errorf("candidate supersession: role %q has no recovery authority", f.Role)
	}
	if f.ReportedLease <= 0 || f.LeaseGeneration != f.ReportedLease || !f.LeaseLive {
		return fmt.Errorf("candidate supersession: stale or non-live lease token/generation")
	}
	if strings.TrimSpace(f.SessionID) == "" || f.SessionID != f.LaunchSession || strings.TrimSpace(f.BuilderSession) == "" {
		return fmt.Errorf("candidate supersession: builder session is not the active signed task-bound launch")
	}
	if role == dispatch.RoleWorker && !f.LiveLaunch {
		return fmt.Errorf("candidate supersession: normal worker authority requires its exact live launch")
	}
	if role == dispatch.RoleWorker && f.AuthorityScope != "" {
		return fmt.Errorf("candidate supersession: worker authority must not carry a recovery scope")
	}
	if role == dispatch.RoleRecovery && (f.AuthorityScope != dispatch.AuthorityScopeCandidateSupersession ||
		!equalFold(f.LeaseTaskRef, f.TaskRef+":"+dispatch.RoleRecovery) || f.AuthorizedCandidateSHA != f.ReplacementSHA) {
		return fmt.Errorf("candidate supersession: coordinator-issued recovery receipt is not bound to the exact recovery lease and replacement")
	}
	if strings.TrimSpace(f.Model) == "" || f.Model != f.LaunchModel ||
		strings.TrimSpace(f.Family) == "" || f.Family != f.LaunchFamily {
		return fmt.Errorf("candidate supersession: builder model/family provenance mismatch")
	}
	if strings.TrimSpace(f.Branch) == "" || f.Branch != f.GitBranch {
		return fmt.Errorf("candidate supersession: signed branch does not match worktree branch")
	}
	if strings.TrimSpace(f.Worktree) == "" || f.Worktree != f.RegisteredWorktree ||
		!strings.HasPrefix(filepath.ToSlash(f.Worktree), "./.herd/worktrees/") {
		return fmt.Errorf("candidate supersession: checkout is not the exact contained registered task worktree")
	}
	if !validShotSHA(f.BaseSHA) || f.BaseSHA != f.GitBaseSHA {
		return fmt.Errorf("candidate supersession: immutable signed base mismatch")
	}
	if !validShotSHA(f.ReplacementSHA) || f.GitHeadSHA != f.ReplacementSHA {
		return fmt.Errorf("candidate supersession: replacement is not exact worktree HEAD")
	}
	if !f.Clean {
		return fmt.Errorf("candidate supersession: task worktree is dirty")
	}
	if !f.ReplacementReachable {
		return fmt.Errorf("candidate supersession: replacement SHA is not reachable from the signed base and branch")
	}
	return nil
}

func supersedeShotLifecycleCandidate(ctx context.Context, root, ref string, lease int64, sha string, machine *lifecycle.Machine, current *lifecycle.TaskState) error {
	facts, authority, err := collectShotSupersessionFacts(ctx, root, ref, lease, sha)
	if err != nil {
		return err
	}
	if err := validateShotSupersessionFacts(facts); err != nil {
		return err
	}
	if current == nil || current.State != lifecycle.StateRecovering || current.CandidateSHA == "" || current.CandidateSHA == sha {
		return fmt.Errorf("candidate supersession: lifecycle must hold a distinct candidate in Recovering")
	}
	evidenceJSON, err := lifecycle.EncodeCandidateSupersessionEvidence(facts)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(evidenceJSON)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	result, err := machine.SupersedeCandidate(lifecycle.CandidateSupersessionRequest{
		TaskRef: ref, TaskID: authority.TaskID, ProjectID: authority.ProjectID, Repo: current.Repo,
		ExpectedSequence: current.Seq, LeaseGeneration: lease, Branch: authority.Branch,
		BaseSHA: authority.BaseSHA, OldCandidateSHA: current.CandidateSHA, NewCandidateSHA: sha,
		Worktree: facts.Worktree, Actor: authority.SessionID, BuilderSession: facts.BuilderSession,
		BuilderModel: facts.Model, BuilderFamily: facts.Family, EvidenceDigest: digest,
		IdempotencyKey: fmt.Sprintf("shot:%s:lease:%d:supersede:%s", strings.ToLower(ref), lease, sha),
	})
	if err != nil {
		return fmt.Errorf("shot: supersede recovering lifecycle candidate: %w", err)
	}
	if result.Event.CandidateSHA != sha || result.Event.Seq != current.Seq+1 {
		return fmt.Errorf("shot: candidate supersession returned non-exact readback")
	}
	return nil
}

func collectShotSupersessionFacts(ctx context.Context, root, ref string, lease int64, sha string) (shotSupersessionFacts, dispatch.TaskContext, error) {
	var facts shotSupersessionFacts
	facts.ReportedRef, facts.ReportedLease, facts.ReplacementSHA = ref, lease, sha
	cwd, err := os.Getwd()
	if err != nil {
		return facts, dispatch.TaskContext{}, fmt.Errorf("candidate supersession: resolve worktree cwd: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return facts, dispatch.TaskContext{}, fmt.Errorf("candidate supersession: resolve worktree path: %w", err)
	}
	authority, err := dispatch.ReadTaskContext(cwd)
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: read signed task context: %w", err)
	}
	facts.ProviderType, facts.ProjectID = authority.ProviderType, authority.ProjectID
	facts.TaskRef, facts.TaskID, facts.Role, facts.AuthorityScope = authority.TaskRef, authority.TaskID, authority.Role, authority.AuthorityScope
	facts.LeaseGeneration, facts.SessionID = authority.LeaseGeneration, authority.SessionID
	facts.LeaseTaskRef, facts.AuthorizedCandidateSHA = authority.LeaseTaskRef, authority.CandidateSHA
	facts.Branch, facts.BaseSHA = authority.Branch, authority.BaseSHA

	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: load receipt verifier: %w", err)
	}
	if err := verifier.Verify(authority); err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: verify signed task context: %w", err)
	}
	facts.ReceiptVerified = true
	canonical, err := dispatch.LoadCanonicalReceiptSession(root, authority.ProviderType, authority.ProjectID, authority.TaskRef, authority.SessionID)
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: load canonical task context: %w", err)
	}
	facts.CanonicalReceiptMatches = canonical.EqualsIssued(authority)
	facts.LaunchSession = canonical.SessionID
	if err := authority.Authorize(currentTime(), provider.OpGet); err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: task context is not active: %w", err)
	}
	if err := requireLiveLease(ctx, root, authority); err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: lease prevalidation: %w", err)
	}
	facts.LeaseLive = true

	cfg, err := config.LoadConfig(shotConfigPath(root))
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: load config: %w", err)
	}
	facts.ConfiguredProviderType, facts.ConfiguredProjectID = cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: task provider: %w", err)
	}
	task, err := tp.GetTask(ctx, ref)
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: provider readback UNKNOWN: %w", err)
	}
	if task == nil {
		return facts, authority, fmt.Errorf("candidate supersession: provider readback returned no task")
	}
	facts.ProviderTaskRef, facts.ProviderTaskID, facts.ProviderTaskProjectID, facts.ProviderStatus = task.Ref, task.ID, task.ProjectID, task.Status

	portable, registered, err := shotRegisteredWorktree(ctx, root, cwd)
	if err != nil {
		return facts, authority, err
	}
	facts.Worktree, facts.RegisteredWorktree = portable, registered
	facts.GitBranch, err = shotGit(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: resolve branch: %w", err)
	}
	facts.GitHeadSHA, err = shotGit(ctx, cwd, "rev-parse", "HEAD")
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: resolve HEAD: %w", err)
	}
	facts.GitBaseSHA, err = shotGit(ctx, cwd, "rev-parse", authority.BaseSHA+"^{commit}")
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: resolve signed base: %w", err)
	}
	status, err := shotGit(ctx, cwd, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return facts, authority, fmt.Errorf("candidate supersession: inspect worktree cleanliness: %w", err)
	}
	facts.Clean = status == ""
	baseReachable := commitIsAncestor(cwd, authority.BaseSHA, sha)
	branchReachable := commitIsAncestor(cwd, sha, authority.Branch)
	commitExists := shotGitOK(ctx, cwd, "cat-file", "-e", sha+"^{commit}")
	facts.ReplacementReachable = baseReachable && branchReachable && commitExists

	commitTime := committime.Of(cwd, sha)
	if commitTime.IsZero() {
		return facts, authority, fmt.Errorf("candidate supersession: resolve replacement committer time")
	}
	receipt, err := exactShotLaunchReceipt(root, ref, authority.Branch, cwd, sha, commitTime)
	if err != nil {
		return facts, authority, err
	}
	facts.Model, facts.LaunchModel = receipt.Model, receipt.Model
	facts.BuilderSession = receipt.Name
	facts.Family = receipt.BuilderFamily
	facts.LaunchFamily = router.FamilyFor(receipt.Provider, receipt.Model)
	facts.LiveLaunch = exactLiveShotAgent(receipt, authority, cwd)
	return facts, authority, nil
}

func currentTime() (now time.Time) { return time.Now() }

func shotGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func shotGitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := shotGit(ctx, dir, args...)
	return err == nil
}

func shotRegisteredWorktree(ctx context.Context, root, cwd string) (string, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("candidate supersession: canonicalize repository root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("candidate supersession: canonicalize repository root: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", "", fmt.Errorf("candidate supersession: canonicalize worktree: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", "", fmt.Errorf("candidate supersession: canonicalize worktree: %w", err)
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("candidate supersession: checkout is outside the canonical repository worktree pool")
	}
	portable := "./" + filepath.ToSlash(rel)
	pool, err := filepath.EvalSymlinks(filepath.Join(root, ".herd", "worktrees"))
	if err != nil {
		return portable, "", fmt.Errorf("candidate supersession: canonicalize worktree pool: %w", err)
	}
	poolRel, err := filepath.Rel(pool, cwd)
	if err != nil || poolRel == "." || poolRel == ".." || strings.HasPrefix(poolRel, ".."+string(filepath.Separator)) {
		return portable, "", fmt.Errorf("candidate supersession: checkout escapes the canonical task worktree pool")
	}
	out, err := shotGit(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return portable, "", fmt.Errorf("candidate supersession: list registered worktrees: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		candidate := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		candidate, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if candidate == cwd {
			return portable, portable, nil
		}
	}
	return portable, "", fmt.Errorf("candidate supersession: checkout is not registered by git worktree")
}

func exactShotLaunchReceipt(root, ref, branch, cwd, candidateSHA string, commitTime time.Time) (launch.Receipt, error) {
	if !validShotSHA(candidateSHA) || commitTime.IsZero() {
		return launch.Receipt{}, fmt.Errorf("candidate supersession: exact candidate and committer time are required for builder provenance")
	}
	receipts, err := launch.ReadReceipts(launch.ReceiptPathFor(root))
	if err != nil {
		return launch.Receipt{}, fmt.Errorf("candidate supersession: read launch receipts: %w", err)
	}
	var found launch.Receipt
	for _, receipt := range receipts {
		role := strings.ToLower(strings.TrimSpace(receipt.Role))
		builderRole := role == launch.WorkerRole
		if !receipt.Accepted || !strings.EqualFold(strings.TrimSpace(receipt.TaskRef), strings.TrimSpace(ref)) ||
			(receipt.Branch != branch) || !builderRole || strings.TrimSpace(receipt.CWD) != cwd ||
			receipt.CreatedAt.IsZero() || receipt.CreatedAt.After(commitTime) {
			continue
		}
		if recorded := strings.TrimSpace(receipt.CandidateSHA); recorded != "" && recorded != candidateSHA {
			continue
		}
		if strings.TrimSpace(receipt.Name) == "" || strings.TrimSpace(receipt.Provider) == "" ||
			strings.TrimSpace(receipt.Model) == "" || strings.TrimSpace(receipt.BuilderFamily) == "" {
			continue
		}
		if !found.CreatedAt.IsZero() && receipt.CreatedAt.Before(found.CreatedAt) {
			continue
		}
		found = receipt
	}
	if found.CreatedAt.IsZero() {
		return launch.Receipt{}, fmt.Errorf("candidate supersession: no exact accepted builder launch receipt for task/branch/worktree")
	}
	return found, nil
}

func exactLiveShotAgent(receipt launch.Receipt, authority dispatch.TaskContext, cwd string) bool {
	agents, err := herdr.AgentList()
	if err != nil {
		return false
	}
	for _, agent := range agents {
		if agent.Name != receipt.Name || agent.Cwd != cwd || !strings.EqualFold(agent.Kind, receipt.Provider) {
			continue
		}
		if authority.HerdrWorkspace != "" && agent.Workspace != authority.HerdrWorkspace {
			continue
		}
		return true
	}
	return false
}
