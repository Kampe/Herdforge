package harvest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// IntegrationBatch is retained with the exact-pass record. Discovery is not a
// resume authority: after a merge these commits disappear from git cherry,
// but their runtime binding, content proof, and cleanup still need to run.
// Worktree is relative to the Git common-directory parent, not a lane root.
type IntegrationBatch struct {
	Candidate string   `json:"candidate"`
	Task      string   `json:"task"`
	Branch    string   `json:"branch"`
	Worktree  string   `json:"worktree"`
	Sources   []string `json:"sources"`
}

// IntegrationSteps owns the native per-step effects, including PR publication
// and actual runtime revision binding. A remote-tracking ref update is neither.
// Each operation receives the immutable, independently admitted source batch.
// Check and Observe must be read-only. The integration driver serializes these
// callbacks and persists the operation identity before any Execute call.
type IntegrationSteps interface {
	Check(context.Context, IntegrationBatch, integration.Transaction, integration.Intent) error
	Observe(context.Context, IntegrationBatch, integration.Transaction, integration.Intent) (integration.Observation, error)
	Execute(context.Context, IntegrationBatch, integration.Transaction, integration.Intent) error
}

// RunCandidate advances exactly the explicitly requested step of one candidate.
// It does not delegate to the old all-at-once Run/runMergeBatch pipeline.
// All native steps must be supplied; there is no permissive legacy fallback.
func (in *Integration) RunCandidate(ctx context.Context, sha string, expected integration.Step, steps IntegrationSteps) (*IntegrationResult, error) {
	if steps == nil {
		return nil, fmt.Errorf("integration: native PR/merge/runtime/proof/cleanup steps are required")
	}
	candidate, err := integration.New(sha)
	if err != nil || len(strings.TrimSpace(sha)) != 40 {
		return nil, fmt.Errorf("integration: full exact candidate required: %q", sha)
	}
	sha = candidate.Candidate
	root, err := worktree.ResolveCanonicalRoot(ctx, in.RepoRoot, "")
	if err != nil {
		return nil, err
	}
	tx, err := integration.Load(root, sha)
	if err != nil {
		return nil, err
	}
	res := &IntegrationResult{}
	var batch IntegrationBatch
	if len(tx.Done) != 0 {
		if tx.DriverVersion != 1 || tx.Done[0].Step != integration.StepPass {
			return nil, fmt.Errorf("integration: manual history cannot supply an executable source batch")
		}
		if err := json.Unmarshal([]byte(tx.Done[0].Evidence), &batch); err != nil {
			return nil, fmt.Errorf("integration: retained source batch is unreadable: %w", err)
		}
	} else {
		if in.Harvester == nil {
			return nil, fmt.Errorf("integration: candidate discovery is unavailable")
		}
		// No fetch or writes merely to decide which exact PASS to admit.
		hr, err := in.Harvester.HarvestReadOnly(ctx)
		if err != nil {
			return nil, err
		}
		res.HarvestResult = hr
		for _, uw := range hr.UnmergedWorktrees {
			for _, source := range uw.Unmerged {
				if source != sha {
					continue
				}
				if batch.Candidate != "" {
					return nil, fmt.Errorf("integration: candidate appears in multiple worktrees")
				}
				rel, err := filepath.Rel(root, uw.WorktreePath)
				if err != nil {
					return nil, err
				}
				batch = IntegrationBatch{Candidate: sha, Branch: uw.Branch, Worktree: filepath.ToSlash(rel), Sources: append([]string(nil), uw.Unmerged...)}
			}
		}
		if batch.Candidate == "" {
			return nil, fmt.Errorf("integration: exact candidate %s has no discovered worktree or retained transaction", sha)
		}
		if in.AdmissionSource == nil {
			return nil, fmt.Errorf("integration: missing admission context source")
		}
		adm, err := in.AdmissionSource.ForCandidate(ctx, sha, batch.unmerged(root))
		if err != nil {
			return nil, err
		}
		batch.Task = adm.Task
	}
	if err := batch.validate(sha); err != nil {
		return nil, err
	}
	backend := &candidateBackend{in: in, root: root, batch: batch, steps: steps, result: res}
	if in.DryRun {
		if err := integration.CheckActiveCandidate(root, sha); err != nil {
			return nil, err
		}
		next, more := tx.Next()
		if !more || expected != next {
			return nil, fmt.Errorf("integration: requested preview %q, next step is %q", expected, next)
		}
		// No intent is generated, no transaction is written, and no effect is
		// observed or executed during a dry-run. Live gates alone are checked.
		preview := integration.Intent{Candidate: sha, Step: expected}
		if tx.Pending != nil {
			preview = *tx.Pending
		}
		if err := backend.Check(ctx, *tx, preview); err != nil {
			return nil, err
		}
		res.Preview = &preview
		return res, nil
	}
	record, err := integration.AdvanceActive(ctx, root, sha, expected, backend)
	if err != nil {
		return nil, err
	}
	res.Progress = record
	return res, nil
}

func (b IntegrationBatch) validate(sha string) error {
	if b.Candidate != sha || strings.TrimSpace(b.Task) == "" || strings.TrimSpace(b.Branch) == "" || b.Worktree == "" || filepath.IsAbs(b.Worktree) || len(b.Sources) == 0 {
		return fmt.Errorf("integration: incomplete or mismatched retained source identity")
	}
	if filepath.ToSlash(filepath.Clean(b.Worktree)) != b.Worktree || b.Worktree == "." {
		return fmt.Errorf("integration: source worktree must be a canonical relative lane path")
	}
	seen := make(map[string]bool, len(b.Sources))
	for _, sha := range b.Sources {
		c, err := integration.New(sha)
		if err != nil || len(sha) != 40 || c.Candidate != sha || seen[sha] {
			return fmt.Errorf("integration: invalid or duplicate batch source %q", sha)
		}
		seen[sha] = true
	}
	if b.Sources[len(b.Sources)-1] != b.Candidate {
		return fmt.Errorf("integration: batch does not end at the exact reviewed candidate")
	}
	return nil
}

func (b IntegrationBatch) unmerged(root string) UnmergedWork {
	return UnmergedWork{WorktreePath: filepath.Join(root, filepath.FromSlash(b.Worktree)), Branch: b.Branch, Unmerged: append([]string(nil), b.Sources...)}
}

func copyBatch(b IntegrationBatch) IntegrationBatch {
	b.Sources = append([]string(nil), b.Sources...)
	return b
}

type candidateBackend struct {
	in     *Integration
	root   string
	batch  IntegrationBatch
	steps  IntegrationSteps
	result *IntegrationResult
}

func (b *candidateBackend) Check(ctx context.Context, tx integration.Transaction, intent integration.Intent) error {
	switch intent.Step {
	case integration.StepPass, integration.StepHarvest, integration.StepIntegrationPR, integration.StepMerge:
		uw := b.batch.unmerged(b.root)
		common, err := worktree.GitCommonDir(ctx, uw.WorktreePath)
		if err != nil {
			return err
		}
		projectCommon, err := worktree.GitCommonDir(ctx, b.root)
		if err != nil || common != projectCommon {
			return fmt.Errorf("integration: source worktree changed repository identity")
		}
		head, err := gitOutput(ctx, uw.WorktreePath, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(head) != b.batch.Candidate {
			return fmt.Errorf("integration: source worktree no longer has the exact candidate HEAD")
		}
		for _, sha := range b.batch.Sources {
			rg := b.in.runReviewGate(ctx, sha, uw)
			b.result.ReviewGatedSHAs = append(b.result.ReviewGatedSHAs, rg)
			if !rg.Eligible || rg.Task != b.batch.Task {
				return fmt.Errorf("integration: exact batch admission refused for %s: %s", sha, rg.Reason)
			}
		}
	}
	return b.steps.Check(ctx, copyBatch(b.batch), tx, intent)
}

func (b *candidateBackend) Observe(ctx context.Context, tx integration.Transaction, intent integration.Intent) (integration.Observation, error) {
	if intent.Step == integration.StepPass {
		body, err := json.Marshal(b.batch)
		return integration.Observation{Intent: intent, State: integration.EffectApplied, Evidence: string(body)}, err
	}
	return b.steps.Observe(ctx, copyBatch(b.batch), tx, intent)
}

func (b *candidateBackend) Execute(ctx context.Context, tx integration.Transaction, intent integration.Intent) error {
	if intent.Step == integration.StepPass {
		return fmt.Errorf("integration: exact PASS is a read-only admission, not an external effect")
	}
	return b.steps.Execute(ctx, copyBatch(b.batch), tx, intent)
}
