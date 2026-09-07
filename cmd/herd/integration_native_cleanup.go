package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func (n *nativeIntegrationSteps) proofReceipt(ctx context.Context, p nativeIntegrationPlan, tx integration.Transaction) (*hsync.CompletionReceipt, error) {
	m, err := nativeIntegrationMerged(tx)
	if err != nil {
		return nil, err
	}
	path := hsync.ReceiptPath(n.root, p.Request.Ref)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	r, err := hsync.LoadReceipt(path)
	if err != nil {
		return nil, err
	}
	req, d := m.Admission.Request, m.Admission.Decision
	policy, err := preflight.LoadMergePolicy(n.root)
	if err != nil {
		return nil, err
	}
	if d.PolicyRevision == "" || preflight.PolicyRevision(policy) != d.PolicyRevision {
		return nil, fmt.Errorf("integration: current policy differs from retained completion admission")
	}
	if r.RepoID != p.Repository || r.Digest != r.ComputeDigest() || r.TaskRef != req.Ref || r.TaskID != req.TaskID || r.CandidateSHA != req.CandidateSHA || r.BaseSHA != req.BaseSHA || r.MergeSHA != m.PR.MergeCommit.OID || r.LeaseGeneration != req.LeaseGeneration || r.AcceptanceDigest != req.AcceptanceDigest || r.ProviderRevision != req.ProviderRevision || r.VerificationDigest != d.VerificationDigest || r.RiskTier != d.Tier || r.AuthorFamily != req.AuthorFamily || r.ReviewerFamily != d.ReviewerFam || r.Verdict != "PASS" || r.IntegrationResult != hsync.IntegrationMerged {
		return nil, fmt.Errorf("integration: canonical receipt contradicts retained admission")
	}
	main, err := n.remoteMain(ctx)
	if err != nil {
		return nil, err
	}
	if err := gitroot.RequireAncestorContext(ctx, n.root, r.MergeSHA, main); err != nil {
		return nil, err
	}
	proof, err := mergeadmit.Prove(n.root, mergeadmit.ProofRequest{Mode: d.Mode, BaseSHA: req.BaseSHA, CandidateSHA: req.CandidateSHA, LandedSHA: r.MergeSHA})
	if err != nil || proof.PatchID != r.PatchID {
		return nil, fmt.Errorf("integration: exact receipt content proof refused: %v", err)
	}
	rows, err := n.ledger.AllRows()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Event == string(reviewledger.EventConsumed) && row.SHA == req.CandidateSHA && row.MergeSHA == r.MergeSHA {
			return r, nil
		}
	}
	// A crash after receipt persistence but before consumption still needs the
	// idempotent native Complete call; a file alone cannot finish this step.
	return nil, nil
}

func (n *nativeIntegrationSteps) checkCleanup(ctx context.Context, b harvest.IntegrationBatch, p nativeIntegrationPlan, tx integration.Transaction) error {
	if !tx.Completed(integration.StepPatchProof) {
		return fmt.Errorf("integration: no recorded cleanup authority")
	}
	r, err := n.proofReceipt(ctx, p, tx)
	if err != nil || r == nil {
		return fmt.Errorf("integration: cleanup proof receipt unavailable: %v", err)
	}
	for _, record := range tx.Done {
		if record.Step == integration.StepPatchProof {
			var recorded hsync.CompletionReceipt
			if err := json.Unmarshal([]byte(record.Evidence), &recorded); err != nil || recorded.Digest != r.Digest {
				return fmt.Errorf("integration: cleanup receipt differs from recorded proof")
			}
		}
	}
	cfg, err := config.LoadConfig(filepath.Join(n.root, ".herd", "herd.yaml"))
	if err != nil {
		return fmt.Errorf("integration: standing-worktree protection unavailable: %w", err)
	}
	for _, rel := range []string{b.Worktree, p.Worktree} {
		path := filepath.Join(n.root, filepath.FromSlash(rel))
		for _, lane := range cfg.Lanes {
			if lane.Worktree == "" {
				continue
			}
			standing := lane.Worktree
			if !filepath.IsAbs(standing) {
				standing = filepath.Join(n.root, standing)
			}
			if nativeIntegrationPath(standing) == nativeIntegrationPath(path) {
				return fmt.Errorf("integration: configured standing worktree is protected")
			}
		}
		if rel == p.Worktree {
			if err := n.integrationWorktreeOwned(ctx, b, p, tx); err != nil {
				return err
			}
		} else if err := worktree.RefuseRemovalWithLiveLease(ctx, n.root, path); err != nil {
			return err
		}
	}
	// Live owner absence must come from a successful native roster. This never
	// closes a reviewer or standing pane to manufacture cleanup eligibility.
	raw, err := nativeIntegrationCommand(ctx, n.root, 15*time.Second, "herdr", "agent", "list")
	if err != nil {
		return err
	}
	var roster struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Agents []herdr.AgentEntry `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &roster); err != nil || roster.Result == nil || roster.Result.Agents == nil || len(roster.Error) > 0 {
		return fmt.Errorf("integration: source-owner census is unknown")
	}
	for _, agent := range roster.Result.Agents {
		cwd := agent.ForegroundCwd
		if cwd == "" {
			cwd = agent.Cwd
		}
		if cwd == "" {
			return fmt.Errorf("integration: live agent has no source-owner location")
		}
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(n.root, cwd)
		}
		for _, source := range []string{b.Worktree, p.Worktree} {
			path := nativeIntegrationPath(filepath.Join(n.root, source))
			current := nativeIntegrationPath(cwd)
			if current == path || strings.HasPrefix(current, path+string(filepath.Separator)) {
				return fmt.Errorf("integration: source still has a resident agent %s; retain its session until owned retirement", agent.Name)
			}
		}
	}
	return nil
}

func nativeIntegrationPath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	return filepath.Clean(path)
}

func (n *nativeIntegrationSteps) cleanupDone(ctx context.Context, b harvest.IntegrationBatch, p nativeIntegrationPlan) (bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	task, err := n.tasks.GetTask(readCtx, p.Request.TaskID)
	if err != nil || task == nil {
		return false, fmt.Errorf("integration: completion readback UNKNOWN: %v", err)
	}
	if task.ID != p.Request.TaskID || task.Ref != b.Task {
		return false, fmt.Errorf("integration: completion readback has wrong identity")
	}
	if provider.NormalizeStatus(task.Status) != provider.StatusDone {
		return false, nil
	}
	for _, pair := range [][2]string{{b.Worktree, b.Branch}, {p.Worktree, p.Branch}} {
		present, err := n.sourcePresent(ctx, pair[0], pair[1], b.Candidate)
		if err != nil || present {
			return false, err
		}
	}
	return true, nil
}
func (n *nativeIntegrationSteps) sourcePresent(ctx context.Context, rel, branch, sha string) (bool, error) {
	if rel == "." || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || branch == "main" || branch == "master" || branch == "" {
		return false, fmt.Errorf("integration: unsafe source cleanup identity")
	}
	path := filepath.Join(n.root, rel)
	registered, err := n.manager().ListWorktrees(ctx)
	if err != nil {
		return false, err
	}
	found := false
	var others []*worktree.WorktreeInfo
	for _, wt := range registered {
		if nativeIntegrationPath(wt.Path) == nativeIntegrationPath(path) {
			if wt.Commit != sha || wt.Branch != branch {
				return false, fmt.Errorf("integration: source registration advanced or changed branch")
			}
			found = true
		} else {
			if wt.Branch == branch {
				return false, fmt.Errorf("integration: cleanup branch is attached elsewhere")
			}
			others = append(others, wt)
		}
	}
	if err := worktree.RejectSharedRoot(n.root, path); err != nil {
		return false, err
	}
	if err := worktree.RejectContainedDestination(n.root, path, others); err != nil {
		return false, err
	}
	if found {
		status, err := nativeIntegrationCommand(ctx, path, 15*time.Second, "git", "status", "--porcelain", gitroot.StatusUntrackedNormal)
		if err != nil || strings.TrimSpace(string(status)) != "" {
			return false, fmt.Errorf("integration: source worktree is dirty or unreadable: %v", err)
		}
		return true, nil
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return false, fmt.Errorf("integration: unregistered source directory requires preservation: %v", err)
	}
	head, err := n.manager().BranchHead(ctx, branch)
	if err != nil {
		return false, err
	}
	if head != "" {
		if head != sha {
			return false, fmt.Errorf("integration: cleanup branch advanced")
		}
		return true, nil
	}
	return false, nil
}
func (n *nativeIntegrationSteps) removeExactSource(ctx context.Context, rel, branch, sha string) error {
	present, err := n.sourcePresent(ctx, rel, branch, sha)
	if err != nil || !present {
		return err
	}
	path := filepath.Join(n.root, rel)
	if _, err := os.Lstat(path); err == nil {
		if err := n.manager().RemoveWorktreeSafely(ctx, path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err = nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "update-ref", "-d", "refs/heads/"+branch, sha)
	return err
}

// Integration worktrees are owned by the driver's retained creation intent,
// not a fabricated builder-dispatch lease. This is the same external-ownership
// distinction as review-pool slots; any actual live claim still fences removal.
func (n *nativeIntegrationSteps) integrationWorktreeOwned(ctx context.Context, b harvest.IntegrationBatch, p nativeIntegrationPlan, tx integration.Transaction) error {
	if err := n.validatePlan(b, p); err != nil {
		return err
	}
	if tx.DriverVersion != 1 || tx.Candidate != b.Candidate || !tx.Completed(integration.StepPatchProof) {
		return fmt.Errorf("integration: no managed proof for integration-worktree retirement")
	}
	for _, r := range tx.Done {
		if r.Step != integration.StepHarvest {
			continue
		}
		i := integration.Intent{Candidate: b.Candidate, ID: r.OperationID, Step: integration.StepHarvest}
		if len(i.ID) != 32 {
			return fmt.Errorf("integration: harvest has no creation operation")
		}
		var retained nativeIntegrationPlan
		exists, err := readNativeIntegrationState(n.root, i, "plan", &retained)
		if err != nil || !exists {
			return fmt.Errorf("integration: creation ownership unavailable: %v", err)
		}
		want, err := json.Marshal(p)
		if err != nil {
			return err
		}
		got, err := json.Marshal(retained)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return fmt.Errorf("integration: creation ownership contradicts retained harvest")
		}
		return worktree.RefuseRemovalWithoutLeaseHistoryCheck(ctx, n.root, filepath.Join(n.root, p.Worktree))
	}
	return fmt.Errorf("integration: no retained integration-worktree creation")
}

func (n *nativeIntegrationSteps) removeIntegrationSource(ctx context.Context, b harvest.IntegrationBatch, p nativeIntegrationPlan, tx integration.Transaction) error {
	if r, err := n.proofReceipt(ctx, p, tx); err != nil || r == nil {
		return fmt.Errorf("integration: retirement proof unavailable: %v", err)
	}
	if err := n.integrationWorktreeOwned(ctx, b, p, tx); err != nil {
		return err
	}
	present, err := n.sourcePresent(ctx, p.Worktree, p.Branch, b.Candidate)
	if err != nil || !present {
		return err
	}
	path := filepath.Join(n.root, p.Worktree)
	if _, err := os.Lstat(path); err == nil {
		if _, err := nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "worktree", "remove", path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err = nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "update-ref", "-d", "refs/heads/"+p.Branch, b.Candidate)
	return err
}

func (n *nativeIntegrationSteps) boardComplete(ctx context.Context, p nativeIntegrationPlan) (err error) {
	cfg, err := config.LoadConfig(filepath.Join(n.root, ".herd", "herd.yaml"))
	if err != nil {
		return err
	}
	if cfg.TaskProvider.ProjectID != n.project {
		return fmt.Errorf("integration: completion project changed")
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	task, err := n.tasks.GetTask(readCtx, p.Request.TaskID)
	cancel()
	if err != nil || task == nil {
		return fmt.Errorf("integration: exact completion task UNKNOWN: %v", err)
	}
	if task.ID != p.Request.TaskID || task.Ref != p.Request.Ref {
		return fmt.Errorf("integration: completion task identity changed")
	}
	req, closeAuthority, err := buildDoneRequest(n.root, n.project, p.Request.Ref, "", "", nil)
	defer closeAuthority()
	if err != nil {
		return err
	}
	dir, err := provider.CanonicalClaimDir(n.root, "")
	if err != nil {
		return err
	}
	stack, err := provider.OpenClaimStack(dir, n.tasks)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := stack.Close(); err == nil {
			err = closeErr
		}
	}()
	result, err := fencedBoardDone(ctx, cfg, n.tasks, stack, task, req)
	if err != nil {
		return err
	}
	releaseScopeClaimQuietly(result.Ref)
	return nil
}
