package next

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

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
	actions := p.evalAll(ctx)
	if len(actions) == 0 {
		return &NextAction{Type: ActionNone, Description: "No action needed."}, nil
	}
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Priority < actions[j].Priority
	})
	return actions[0], nil
}

func (p *NextPicker) EvalAll(ctx context.Context) []*NextAction {
	actions := p.evalAll(ctx)
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Priority < actions[j].Priority
	})
	return actions
}

func (p *NextPicker) evalAll(ctx context.Context) []*NextAction {
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
	harvestReady, rebaseNeeded, _, _, _ := drainSummary(ctx, p.TaskProvider, p.Config)
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
	inReview, needReview := reviewPipelineCounts(ctx, p.TaskProvider, p.Config)
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
			Description: fmt.Sprintf("%d unreviewed tip(s) need reviewer", needReview),
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

	return actions
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

func drainSummary(ctx context.Context, tp provider.TaskProvider, cfg *config.Config) (harvestReady int, rebaseNeeded int, inReview int, needReview int, total int) {
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		return 0, 0, 0, 0, 0
	}
	for _, t := range tasks {
		total++
		switch t.Status {
		case "review":
			inReview++
		case "in-progress":
			needReview++
		}
	}
	return harvestReady, rebaseNeeded, inReview, needReview, total
}

func reviewPipelineCounts(ctx context.Context, tp provider.TaskProvider, cfg *config.Config) (inReview int, needReview int) {
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "")
	if err != nil {
		return 0, 0
	}
	for _, t := range tasks {
		switch t.Status {
		case "review":
			inReview++
		case "in-progress":
			needReview++
		}
	}
	return inReview, needReview
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
