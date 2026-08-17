package next

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/candidateindex"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type Priority int

type ActionType string

const (
	ActionIngest   ActionType = "ingest-verdicts"
	ActionHarvest  ActionType = "harvest-ready"
	ActionRebase   ActionType = "rebase-needed"
	ActionReview   ActionType = "in-review-at-cap"
	ActionNeed     ActionType = "need-review"
	ActionPark     ActionType = "park-pile"
	ActionWindDown ActionType = "wind-down"
	ActionClaim    ActionType = "claim-task"
	ActionNone     ActionType = "nothing-blocking"
)

type NextAction struct {
	Type        ActionType
	Priority    int
	Description string
	Command     string
	AutoSafe    bool
}

type NextPicker struct {
	Config         *config.Config
	TaskProvider   provider.TaskProvider
	ReviewArtifact string
	InboxDir       string
}

func NewNextPicker(cfg *config.Config, tp provider.TaskProvider) *NextPicker {
	return &NextPicker{
		Config:         cfg,
		TaskProvider:   tp,
		ReviewArtifact: ".herd/review/inbox",
		InboxDir:       ".herd/review/inbox",
	}
}

func (p *NextPicker) Eval(ctx context.Context) (*NextAction, error) {
	actions, err := p.evalAll(ctx)
	if err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return &NextAction{Type: ActionNone, Description: "No action needed."}, nil
	}
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Priority < actions[j].Priority
	})
	return actions[0], nil
}

// EvalAll returns ranked next actions. Provider timeout/ambiguous failures
// return an error — never an empty "claim free capacity" success (FAC-150).
func (p *NextPicker) EvalAll(ctx context.Context) ([]*NextAction, error) {
	actions, err := p.evalAll(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Priority < actions[j].Priority
	})
	return actions, nil
}

func (p *NextPicker) evalAll(ctx context.Context) ([]*NextAction, error) {
	var actions []*NextAction

	// Priority 1: Pending verdict artifacts
	if verdicts := p.pendingVerdicts(); len(verdicts) > 0 {
		actions = append(actions, &NextAction{
			Type:        ActionIngest,
			Priority:    1,
			Description: fmt.Sprintf("%d pending verdict artifact(s) to ingest", len(verdicts)),
			Command:     "herd review-ingest",
			AutoSafe:    true,
		})
	}

	// Priority 2: Harvest-ready or rebase-needed from drain state
	harvestReady, rebaseNeeded, _, _, _, err := drainSummary(ctx, p.TaskProvider, p.Config)
	if err != nil {
		return nil, err
	}
	if harvestReady > 0 {
		actions = append(actions, &NextAction{
			Type:        ActionHarvest,
			Priority:    2,
			Description: fmt.Sprintf("%d task(s) ready for harvest", harvestReady),
			Command:     "herd review",
			AutoSafe:    false,
		})
	}
	if rebaseNeeded > 0 {
		actions = append(actions, &NextAction{
			Type:        ActionRebase,
			Priority:    3,
			Description: fmt.Sprintf("%d task(s) need rebase", rebaseNeeded),
			Command:     "herd review --rebase",
			AutoSafe:    false,
		})
	}

	// Priority 4-5: Review pipeline
	inReview, needReview, blockedReview, err := reviewPipelineCounts(ctx, p.TaskProvider, p.Config)
	if err != nil {
		return nil, err
	}
	reviewCap := 3
	if inReview >= reviewCap {
		actions = append(actions, &NextAction{
			Type:        ActionReview,
			Priority:    4,
			Description: fmt.Sprintf("%d tasks in review (cap: %d)", inReview, reviewCap),
			Command:     "herd review",
			AutoSafe:    false,
		})
	}
	if needReview > 0 {
		actions = append(actions, &NextAction{
			Type:        ActionNeed,
			Priority:    5,
			Description: reviewNeedDescription(needReview, blockedReview),
			Command:     "herd review --spawn",
			AutoSafe:    false,
		})
	}

	// Priority 6: Claim new task (default if nothing blocking)
	actions = append(actions, &NextAction{
		Type:        ActionClaim,
		Priority:    100,
		Description: "No blocking actions — claim next pending task",
		Command:     "herd pulse --spawn",
		AutoSafe:    false,
	})

	return actions, nil
}

func (p *NextPicker) pendingVerdicts() []string {
	entries, err := os.ReadDir(p.InboxDir)
	if err != nil {
		return nil
	}
	var verdicts []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-verdict.md") {
			verdicts = append(verdicts, e.Name())
		}
	}
	return verdicts
}

func drainSummary(ctx context.Context, tp provider.TaskProvider, cfg *config.Config) (harvestReady int, rebaseNeeded int, inReview int, needReview int, total int, err error) {
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		// Fail closed: never map provider timeout to zero free capacity (FAC-150).
		return 0, 0, 0, 0, 0, err
	}
	for _, t := range tasks {
		total++
		switch t.Status {
		case "review", "in-review":
			inReview++
		}
	}
	needReview, _, err = countReviewableAndBlockedTips(ctx, tp, cfg, tasks)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return harvestReady, rebaseNeeded, inReview, needReview, total, nil
}

func reviewPipelineCounts(ctx context.Context, tp provider.TaskProvider, cfg *config.Config) (inReview int, needReview int, blockedReview int, err error) {
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		return 0, 0, 0, err
	}
	for _, t := range tasks {
		switch t.Status {
		case "review", "in-review":
			inReview++
		}
	}
	needReview, blockedReview, err = countReviewableAndBlockedTips(ctx, tp, cfg, tasks)
	if err != nil {
		return 0, 0, 0, err
	}
	return inReview, needReview, blockedReview, nil
}

func reviewNeedDescription(needReview, blockedReview int) string {
	if blockedReview == 0 {
		return fmt.Sprintf("%d unreviewed tip(s) need reviewer", needReview)
	}
	return fmt.Sprintf("%d unreviewed tip(s) need reviewer (%d active candidate(s) blocked by provenance/review gates)", needReview, blockedReview)
}

// countReviewableTips excludes planning/standing cards that have no exact
// candidate SHA. Those cards remain visible to the dependency/provenance
// gates, but must not inflate the P5 reviewer signal.
func countReviewableTips(ctx context.Context, tp provider.TaskProvider, cfg *config.Config, tasks []*provider.Task) (int, error) {
	reviewable, _, err := countReviewableAndBlockedTips(ctx, tp, cfg, tasks)
	return reviewable, err
}

// countReviewableAndBlockedTips returns both actionable review tips and active
// candidates held by a review/provenance gate. Keeping the blocked count
// visible prevents the coordinator from mistaking a quiet reviewer queue for
// a healthy claim surface.
func countReviewableAndBlockedTips(ctx context.Context, tp provider.TaskProvider, cfg *config.Config, tasks []*provider.Task) (int, int, error) {
	root, err := os.Getwd()
	if err != nil {
		return 0, 0, err
	}
	idx := candidateindex.New(candidateindex.IndexOptions{RepoRoot: root, Config: cfg, TaskProvider: tp})
	candidates, err := idx.BuildIndex(ctx)
	if err != nil {
		return 0, 0, err
	}
	active := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task != nil && task.Status == "in-progress" {
			active[strings.ToUpper(strings.TrimSpace(task.Ref))] = true
		}
	}
	count, blocked := 0, 0
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.CandidateSHA) == "" || !active[strings.ToUpper(strings.TrimSpace(candidate.Ref))] {
			continue
		}
		ref := strings.ToUpper(strings.TrimSpace(candidate.Ref))
		if seen[ref] {
			continue
		}
		seen[ref] = true
		// PASS/FAIL/BLOCKED candidates already have a review outcome; only
		// pending candidates need a reviewer signal.
		if candidate.State == candidateindex.StatePending {
			count++
		} else if candidate.State == candidateindex.StateBlocked {
			blocked++
		}
	}
	return count, blocked, nil
}

func (a *NextAction) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[P%d] %s\n", a.Priority, a.Description))
	if a.Command != "" {
		sb.WriteString(fmt.Sprintf("  Run: %s\n", a.Command))
	}
	return sb.String()
}

func (p *NextPicker) Selftest(ctx context.Context) error {
	for _, tool := range []string{"herd"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}
	return nil
}
