package next

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/candidateindex"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"

	"github.com/Kampe/Herdforge/pkg/reviewroot"
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
	Role           string
	ReviewArtifact string
	InboxDir       string
	LedgerPath     string
}

func NewNextPicker(cfg *config.Config, tp provider.TaskProvider) *NextPicker {
	return &NextPicker{
		Config:       cfg,
		TaskProvider: tp,
		// FAC-572: resolved through the one review-root resolver, so the picker
		// cannot read a different corpus than review-ingest wrote to.
		ReviewArtifact: reviewroot.Resolve(".").Inbox(),
		InboxDir:       reviewroot.Resolve(".").Inbox(),
		LedgerPath:     reviewledger.DefaultPath(""),
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

	preview, err := PreviewClaimQueue(ctx, p.TaskProvider, p.Config, p.Role)
	if err != nil {
		return nil, err
	}

	// Priority 4-5: Review pipeline. The in-review cap is advisory (FAC-623 /
	// CHA-3174): it must not suppress an independent dependency-ready builder.
	inReview, needReview, blockedReview, err := reviewPipelineCounts(ctx, p.TaskProvider, p.Config)
	if err != nil {
		return nil, err
	}
	reviewCap := 3
	if inReview >= reviewCap && preview.Claimable == 0 {
		actions = append(actions, &NextAction{
			Type:        ActionReview,
			Priority:    4,
			Description: fmt.Sprintf("%d tasks in review (cap: %d)", inReview, reviewCap),
			Command:     "herd review",
			AutoSafe:    false,
		})
	}
	if needReview > 0 && preview.Claimable == 0 {
		actions = append(actions, &NextAction{
			Type:        ActionNeed,
			Priority:    5,
			Description: reviewNeedDescription(needReview, blockedReview),
			Command:     "herd review --spawn",
			AutoSafe:    false,
		})
	}

	claimCommand := "herd pulse --spawn"
	if preview.Claimable == 0 && preview.ProvenanceBlocked > 0 {
		claimCommand = "herd deps migrate"
	}
	actions = append(actions, &NextAction{
		Type:        ActionClaim,
		Priority:    100,
		Description: preview.Description(),
		Command:     claimCommand,
		AutoSafe:    false,
	})

	return actions, nil
}

// ClaimPreview is the pre-claim admission summary. It reports what can be
// proven from board descriptions before lease/worker side effects occur.
type ClaimPreview struct {
	// Role is the filter this preview was computed under, so a zero result can
	// say WHY it is zero instead of implying an empty queue.
	Role              string
	Claimable         int
	ProvenanceBlocked int
	BlockedRefs       []string
	NextRef           string
	NextID            string
}

func (p ClaimPreview) Description() string {
	if p.Claimable == 0 && p.ProvenanceBlocked > 0 {
		return fmt.Sprintf("No claimable task yet — repair provenance for %d blocked card(s): %s", p.ProvenanceBlocked, strings.Join(p.BlockedRefs, ", "))
	}
	if p.Claimable == 0 && p.ProvenanceBlocked == 0 {
		// FAC-623: "No blocking actions - 0 claimable" reads as a healthy idle
		// queue. It is not: it is emitted when NOTHING matched, and an operator
		// reasonably concluded the review-in-progress cap was throttling dispatch.
		// The cap is advisory (priority 4) and never gates claiming; zero here
		// means no pending task passed the role filter at all.
		//
		// Same shape as the rest of this session's defects: an unmatched filter
		// reported as an absence of work.
		if strings.TrimSpace(p.Role) != "" {
			return fmt.Sprintf("0 claimable — no pending task carries the %q role label "+
				"(this is a filter result, not an idle queue; check role labels before assuming capacity)", p.Role)
		}
		return "0 claimable — no pending task matched (this is a filter result, not an idle queue)"
	}
	nextRef := strings.TrimSpace(p.NextRef)
	if p.ProvenanceBlocked == 0 {
		if nextRef != "" {
			return fmt.Sprintf("Claim %s — %d claimable pending task(s)", nextRef, p.Claimable)
		}
		return fmt.Sprintf("No blocking actions — %d claimable pending task(s)", p.Claimable)
	}
	if nextRef != "" {
		return fmt.Sprintf("Claim %s (%d claimable, %d blocked by missing/invalid herd-deps-v1: %s)", nextRef, p.Claimable, p.ProvenanceBlocked, strings.Join(p.BlockedRefs, ", "))
	}
	return fmt.Sprintf("Claim next pending task (%d claimable, %d blocked by missing/invalid herd-deps-v1: %s)", p.Claimable, p.ProvenanceBlocked, strings.Join(p.BlockedRefs, ", "))
}

// PreviewClaimQueue performs the cheap, deterministic portion of claim
// admission. It is not a replacement for deps.ValidateClaim; it exposes
// obvious provenance omissions before a Forge cycle reports a late failure.
func PreviewClaimQueue(ctx context.Context, tp provider.TaskProvider, cfg *config.Config, roles ...string) (ClaimPreview, error) {
	if tp == nil || cfg == nil {
		return ClaimPreview{}, fmt.Errorf("claim preview requires provider and config")
	}
	role := ""
	if len(roles) > 1 {
		return ClaimPreview{}, fmt.Errorf("claim preview accepts at most one role filter")
	}
	if len(roles) == 1 {
		role = strings.TrimSpace(roles[0])
	}
	tasks, err := tp.ListTasks(ctx, cfg.TaskProvider.ProjectID, "to-do")
	if err != nil {
		return ClaimPreview{}, err
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i] == nil || tasks[j] == nil {
			return tasks[i] != nil
		}
		pi, pj := candidateindex.PriorityRank(tasks[i].Priority), candidateindex.PriorityRank(tasks[j].Priority)
		if pi != pj {
			return pi > pj
		}
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})
	preview := ClaimPreview{Role: role}
	for _, task := range tasks {
		if task == nil || strings.TrimSpace(task.Ref) == "" {
			continue
		}
		if role != "" && !taskMatchesRole(task, role) {
			continue
		}
		prov, provErr := deps.ExtractProvenanceFromText(task.Description)
		if provErr != nil || prov == nil || !prov.Present {
			preview.ProvenanceBlocked++
			preview.BlockedRefs = append(preview.BlockedRefs, task.Ref)
			continue
		}
		preview.Claimable++
		if preview.NextRef == "" {
			preview.NextRef = strings.TrimSpace(task.Ref)
			preview.NextID = strings.TrimSpace(task.ID)
		}
	}
	return preview, nil
}

func taskMatchesRole(task *provider.Task, role string) bool {
	if task == nil || len(task.Labels) == 0 {
		return true
	}
	for _, label := range task.Labels {
		if strings.EqualFold(strings.TrimSpace(label), role) {
			return true
		}
	}
	return false
}

func (p *NextPicker) pendingVerdicts() []string {
	entries, err := os.ReadDir(p.InboxDir)
	if err != nil {
		return nil
	}
	admitted := make(map[string]struct{})
	if strings.TrimSpace(p.LedgerPath) != "" {
		ledger := &reviewledger.Ledger{Path: p.LedgerPath}
		if rows, err := ledger.AllRows(); err == nil {
			for _, row := range rows {
				sha := strings.TrimSpace(row.SHA)
				if sha == "" {
					sha = strings.TrimSpace(row.CandidateSHA)
				}
				if sha != "" {
					admitted[strings.ToLower(sha)] = struct{}{}
				}
			}
		}
	}
	var verdicts []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-verdict.md") {
			artifact, err := os.ReadFile(filepath.Join(p.InboxDir, e.Name()))
			if err == nil {
				parsed := reviewingest.Parse(string(artifact))
				if _, ok := admitted[strings.ToLower(strings.TrimSpace(parsed.SHA))]; ok {
					continue
				}
			}
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
