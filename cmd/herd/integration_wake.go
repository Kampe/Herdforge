package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/beat"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/coordinator"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

const integrationWakeAge = 5 * time.Minute

func integrationWakePath(root string) string {
	return filepath.Join(root, ".herd", "integration-wakes.json")
}

// liveIntegrationOwner requires a registered incarnation, not a guessed
// coordinator name. An unknown/reused pane must not receive a merge prompt.
func liveIntegrationOwner(ctx context.Context, root string, run attentionCommand, forDelivery bool) (*coordinator.Registration, error) {
	reg, err := coordinator.Resolve(root)
	if err != nil {
		return nil, err
	}
	if reg.StartedAt.IsZero() || reg.Name == "" || reg.Workspace == "" || reg.TabID == "" || reg.PaneID == "" || reg.TerminalID == "" {
		return nil, fmt.Errorf("integration wake requires a bound coordinator incarnation")
	}
	raw, err := run(ctx, "herdr", "agent", "list")
	if err != nil {
		return nil, fmt.Errorf("integration coordinator roster: %w", err)
	}
	var roster struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Agents []herdr.AgentEntry `json:"agents"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &roster); err != nil {
		return nil, err
	}
	if len(roster.Error) > 0 || roster.Result == nil || roster.Result.Agents == nil {
		return nil, fmt.Errorf("integration coordinator roster is unknown")
	}
	matches := 0
	for _, a := range roster.Result.Agents {
		if a.PaneID == reg.PaneID && a.TabID == reg.TabID && a.TerminalID == reg.TerminalID && a.Workspace == reg.Workspace {
			if a.Kind == "" || a.InteractiveReady == nil || !*a.InteractiveReady {
				return nil, fmt.Errorf("integration coordinator is not interactively ready")
			}
			if forDelivery && a.Status != "idle" && a.Status != "done" {
				return nil, fmt.Errorf("integration coordinator is busy; wake remains pending")
			}
			matches++
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("registered integration coordinator incarnation is not uniquely live")
	}
	return reg, nil
}

// integrationActions accepts only FAC598's complete ready-but-open evidence.
// Unknown evidence is not a withdrawal: the caller must preserve queued work.
func integrationActions(items []attention.CandidateItem, owner *coordinator.Registration) ([]beat.IntegrationAction, error) {
	var actions []beat.IntegrationAction
	for _, item := range items {
		if item.Status == "ready-evidence-unknown" {
			return nil, fmt.Errorf("integration readiness unknown for %s: %s", item.SHA, item.Reason)
		}
		if item.Status != "ready-but-open" {
			continue
		}
		if owner == nil || !item.ReviewReady || item.MergeReady != nil || item.PullRequest <= 0 || strings.TrimSpace(item.Branch) == "" {
			return nil, fmt.Errorf("incomplete ready-but-open identity for %s", item.SHA)
		}
		actions = append(actions, beat.IntegrationAction{
			CandidateSHA: item.SHA, PullRequest: item.PullRequest, Task: item.Task, Owner: owner.Name, Target: owner.PaneID, Session: owner.TerminalID,
			Action: fmt.Sprintf("Run normal integration admission for %s, exact candidate %s on branch %s, PR #%d. If every required gate admits this exact identity, merge it through the native harvest path; otherwise retain the candidate and report the specific gate. This wake grants no merge authority.", item.Task, item.SHA, item.Branch, item.PullRequest),
		})
	}
	return actions, nil
}

func deliverIntegrationWake(ctx context.Context, root string, w beat.IntegrationWake) error {
	owner, err := liveIntegrationOwner(ctx, root, attentionCommandAt(root), true)
	if err != nil {
		return err
	}
	if owner.Name != w.Owner || owner.PaneID != w.Target || owner.TerminalID != w.Session {
		return fmt.Errorf("integration wake owner incarnation changed before delivery")
	}
	request, err := integrationDeliveryRequest(root, w)
	if err != nil {
		return err
	}
	proof, err := herdr.DeliverOperator(ctx, request)
	if err != nil {
		return err
	}
	if !proof.Verified || !proof.Consumed {
		return fmt.Errorf("integration wake delivery lacks consumption proof")
	}
	return nil
}

// runForgeIntegrationWakes is wired into every production forge tick. It
// prompts the coordinator; it never executes Action, merges, or updates cards.
func runForgeIntegrationWakes(ctx context.Context, cfg *config.Config, tp provider.TaskProvider) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	root, err := canonicalHerdRoot()
	if err != nil {
		return err
	}
	run := attentionCommandAt(root)
	items, err := collectAttentionCandidates(ctx, root, cfg, tp, run)
	if err != nil {
		return fmt.Errorf("integration readiness snapshot: %w", err)
	}
	var owner *coordinator.Registration
	for _, item := range items {
		if item.Status == "ready-but-open" {
			owner, err = liveIntegrationOwner(ctx, root, run, false)
			if err != nil {
				return err
			}
			break
		}
	}
	actions, err := integrationActions(items, owner)
	if err != nil {
		return err
	}
	_, err = beat.ReconcileIntegrationWakes(ctx, integrationWakePath(root), actions, time.Now().UTC(), integrationWakeAge, func(ctx context.Context, w beat.IntegrationWake) error { return deliverIntegrationWake(ctx, root, w) })
	return err
}

// integrationDeliveryRequest binds a stable payload to one candidate generation.
func integrationDeliveryRequest(root string, w beat.IntegrationWake) (herdr.OperatorDelivery, error) {
	payload, err := json.Marshal(struct {
		Event string               `json:"event"`
		Wake  beat.IntegrationWake `json:"wake"`
		Ack   string               `json:"acknowledgement"`
	}{"integration-ready", w, fmt.Sprintf("herd integration-wake --candidate %s --generation %d", w.CandidateSHA, w.Generation)})
	if err != nil {
		return herdr.OperatorDelivery{}, err
	}
	return herdr.OperatorDelivery{
		Key: fmt.Sprintf("integration-ready:%s:%d", w.CandidateSHA, w.Generation), Generation: w.Generation, Target: w.Target, Session: w.Session, Wait: true,
		Payload: textdelivery.Payload{Bytes: payload}, StatePath: herdr.OperatorDeliveryStatePath(root), Timeout: 10 * time.Second}, nil
}
