package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// authenticatedRecoveryIdentity returns the immutable identity carried by the
// existing signed receipt. Recovery issuance must derive from this authority;
// current origin/main is ambient repository state and is never a substitute
// for the builder's authenticated base.
func authenticatedRecoveryIdentity(ctx context.Context, root, targetDir, ref, branch, candidate string, cfg *config.Config, task *provider.Task) (dispatch.TaskContext, error) {
	var zero dispatch.TaskContext
	if cfg == nil || task == nil {
		return zero, fmt.Errorf("recovery receipt: configured project and provider task are required")
	}
	targetDir, err := filepath.Abs(targetDir)
	if err != nil {
		return zero, fmt.Errorf("recovery receipt: resolve worktree: %w", err)
	}
	targetDir, err = filepath.EvalSymlinks(targetDir)
	if err != nil {
		return zero, fmt.Errorf("recovery receipt: canonicalize worktree: %w", err)
	}
	if _, _, err := shotRegisteredWorktree(ctx, root, targetDir); err != nil {
		return zero, fmt.Errorf("recovery receipt: %w", err)
	}

	prior, err := dispatch.ReadTaskContext(targetDir)
	if err != nil {
		return zero, fmt.Errorf("recovery receipt: read prior task context: %w", err)
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return zero, fmt.Errorf("recovery receipt: load verifier: %w", err)
	}
	if err := verifier.Verify(prior); err != nil {
		return zero, fmt.Errorf("recovery receipt: authenticate prior task context: %w", err)
	}
	canonical, err := dispatch.LoadCanonicalReceiptSession(root, prior.ProviderType, prior.ProjectID, prior.TaskRef, prior.SessionID)
	if err != nil {
		return zero, fmt.Errorf("recovery receipt: load prior canonical task context: %w", err)
	}
	if !canonical.EqualsIssued(prior) {
		return zero, fmt.Errorf("recovery receipt: prior worktree and canonical task contexts differ")
	}

	role := strings.ToLower(strings.TrimSpace(prior.Role))
	if role != dispatch.RoleWorker && role != dispatch.RoleRecovery {
		return zero, fmt.Errorf("recovery receipt: prior role %q is not worker/recovery provenance", prior.Role)
	}
	if role == dispatch.RoleRecovery && prior.AuthorityScope != dispatch.AuthorityScopeCandidateSupersession {
		return zero, fmt.Errorf("recovery receipt: generic recovery sentinel is not candidate-supersession provenance")
	}
	repository := dispatch.RepositoryIdentityOrName(root, cfg.Project.Name)
	if prior.Repository != repository || prior.ProviderType != cfg.TaskProvider.Type ||
		prior.ProjectID == "" || prior.ProjectID != cfg.TaskProvider.ProjectID {
		return zero, fmt.Errorf("recovery receipt: prior repository/provider/project identity mismatch")
	}
	if prior.ProviderWorkspace != cfg.TaskProvider.WorkspaceID || prior.ProviderProfile != cfg.TaskProvider.APIKeyEnv {
		return zero, fmt.Errorf("recovery receipt: prior provider workspace/profile identity mismatch")
	}
	if !strings.EqualFold(strings.TrimSpace(prior.TaskRef), strings.TrimSpace(ref)) ||
		!strings.EqualFold(strings.TrimSpace(task.Ref), strings.TrimSpace(ref)) ||
		prior.TaskID == "" || prior.TaskID != task.ID {
		return zero, fmt.Errorf("recovery receipt: prior and provider task identity mismatch")
	}
	if task.ProjectID == "" || task.ProjectID != prior.ProjectID {
		return zero, fmt.Errorf("recovery receipt: provider task project identity mismatch")
	}
	status := provider.NormalizeStatus(task.Status)
	if status == provider.StatusUnknown || strings.HasPrefix(status, provider.StatusUnknown+":") {
		return zero, fmt.Errorf("recovery receipt: provider task state is UNKNOWN")
	}
	if prior.Branch == "" || prior.Branch != branch {
		return zero, fmt.Errorf("recovery receipt: immutable branch mismatch")
	}
	if !validShotSHA(prior.BaseSHA) || !validShotSHA(candidate) {
		return zero, fmt.Errorf("recovery receipt: immutable base or candidate is not an exact commit SHA")
	}
	base, err := shotGit(ctx, targetDir, "rev-parse", prior.BaseSHA+"^{commit}")
	if err != nil || base != prior.BaseSHA {
		return zero, fmt.Errorf("recovery receipt: authenticated immutable base is not present in the worktree")
	}
	head, err := shotGit(ctx, targetDir, "rev-parse", "HEAD")
	if err != nil || head != candidate {
		return zero, fmt.Errorf("recovery receipt: candidate is not exact worktree HEAD")
	}
	if !commitIsAncestor(targetDir, prior.BaseSHA, candidate) ||
		!commitIsAncestor(targetDir, candidate, branch) {
		return zero, fmt.Errorf("recovery receipt: candidate is not reachable from the authenticated base and branch")
	}
	statusOut, err := shotGit(ctx, targetDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil || statusOut != "" {
		return zero, fmt.Errorf("recovery receipt: worktree is not clean")
	}
	return prior, nil
}

type canonicalReceiptStore func(string, dispatch.TaskContext) error

// persistRecoveryReceipt publishes the local and canonical copies as one
// compensated operation. A failure after the canonical rename removes only
// the exact newly-issued session and restores the authenticated prior local
// context, so callers never observe a half-issued recovery authority.
func persistRecoveryReceipt(root, targetDir string, receipt, prior dispatch.TaskContext, store canonicalReceiptStore) error {
	if err := dispatch.WriteTaskContext(targetDir, receipt); err != nil {
		return err
	}
	if err := store(root, receipt); err != nil {
		problems := []string{err.Error()}
		if cleanupErr := dispatch.RemoveCanonicalReceiptSessionIfExact(root, receipt); cleanupErr != nil {
			problems = append(problems, "canonical compensation failed: "+cleanupErr.Error())
		}
		if restoreErr := dispatch.WriteTaskContext(targetDir, prior); restoreErr != nil {
			problems = append(problems, "TASK-CONTEXT compensation failed: "+restoreErr.Error())
		}
		return fmt.Errorf("recovery receipt persistence failed: %s", strings.Join(problems, "; "))
	}
	return nil
}
