package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
)

var ErrRecoveryActive = errors.New("dispatch: stale run recovery refused by live authority")

// LiveLaunchLookup proves whether an admitted launch for one exact task is
// still represented by a live Herdr identity.
type LiveLaunchLookup interface {
	HasLiveLaunch(context.Context, string, string) (bool, error)
}

type LiveLaunchLookupFunc func(context.Context, string, string) (bool, error)

func (f LiveLaunchLookupFunc) HasLiveLaunch(ctx context.Context, taskID, taskRef string) (bool, error) {
	return f(ctx, taskID, taskRef)
}

type receiptLiveLaunchLookup struct {
	path string
	list func() ([]herdr.AgentEntry, error)
}

type receiptLaunchPhase uint8

const (
	receiptPhasePreEdit receiptLaunchPhase = iota + 1
	receiptPhasePostLaunch
)

type receiptLaunchEvidence struct {
	receipt   launch.Receipt
	phase     receiptLaunchPhase
	workspace string
}

func NewReceiptLiveLaunchLookup(path string, list func() ([]herdr.AgentEntry, error)) LiveLaunchLookup {
	if list == nil {
		list = herdr.AgentList
	}
	return &receiptLiveLaunchLookup{path: path, list: list}
}

func (l *receiptLiveLaunchLookup) HasLiveLaunch(_ context.Context, taskID, taskRef string) (bool, error) {
	if l == nil || strings.TrimSpace(l.path) == "" || strings.TrimSpace(taskID) == "" || strings.TrimSpace(taskRef) == "" || l.list == nil {
		return false, fmt.Errorf("dispatch: incomplete live launch authority")
	}
	receipts, err := readLaunchReceiptsStrict(l.path)
	if err != nil {
		return false, err
	}
	candidates := make([]receiptLaunchEvidence, 0)
	for _, receipt := range receipts {
		if !receipt.Accepted || receipt.TaskRef != taskRef {
			continue
		}
		evidence, err := classifyReceiptLaunchEvidence(receipt)
		if err != nil {
			return false, fmt.Errorf("dispatch: accepted launch receipt for %s is UNKNOWN: %w", taskRef, err)
		}
		candidates = append(candidates, evidence)
	}
	if len(candidates) == 0 {
		return false, fmt.Errorf("dispatch: no accepted launch receipt proves exact task %s", taskRef)
	}
	agents, err := l.list()
	if err != nil {
		return false, err
	}
	if agents == nil {
		return false, fmt.Errorf("dispatch: Herdr agent inventory is UNKNOWN")
	}
	matched := make(map[string]struct{})
	for _, agent := range agents {
		related := make([]receiptLaunchEvidence, 0)
		for _, candidate := range candidates {
			if strings.TrimSpace(agent.Name) != strings.TrimSpace(candidate.receipt.Name) {
				continue
			}
			if strings.TrimSpace(agent.Cwd) == "" {
				return false, fmt.Errorf("dispatch: matching Herdr agent %s has UNKNOWN worktree identity", candidate.receipt.Name)
			}
			if !sameWorktree(agent.Cwd, candidate.receipt.CWD) {
				continue
			}
			related = append(related, candidate)
		}
		if len(related) == 0 {
			continue
		}
		identity, err := exactHerdrAgentIdentity(agent)
		if err != nil {
			return false, err
		}
		admitted := false
		for _, candidate := range related {
			if candidate.phase == receiptPhasePreEdit || postLaunchIdentityMatches(candidate, agent) {
				admitted = true
				break
			}
		}
		if !admitted {
			return false, fmt.Errorf("dispatch: live Herdr identity for %s does not match an exact admitted post-launch receipt", taskRef)
		}
		matched[identity] = struct{}{}
	}
	if len(matched) > 1 {
		return false, fmt.Errorf("dispatch: accepted launch receipt for %s matches ambiguous live Herdr identities", taskRef)
	}
	return len(matched) == 1, nil
}

func classifyReceiptLaunchEvidence(receipt launch.Receipt) (receiptLaunchEvidence, error) {
	name := strings.TrimSpace(receipt.Name)
	cwd := strings.TrimSpace(receipt.CWD)
	if name == "" || cwd == "" {
		return receiptLaunchEvidence{}, fmt.Errorf("missing exact name or worktree")
	}
	tabID := strings.TrimSpace(receipt.TabID)
	paneID := strings.TrimSpace(receipt.PaneID)
	session := strings.TrimSpace(receipt.HerdrSession)
	claimsPostLaunch := tabID != "" || paneID != "" || session != ""
	if !claimsPostLaunch {
		if receipt.CreatedAt.IsZero() || strings.TrimSpace(receipt.Branch) == "" ||
			strings.TrimSpace(receipt.Provider) == "" || strings.TrimSpace(receipt.Model) == "" ||
			strings.TrimSpace(receipt.BuilderFamily) == "" {
			return receiptLaunchEvidence{}, fmt.Errorf("incomplete pre-edit provenance")
		}
		return receiptLaunchEvidence{receipt: receipt, phase: receiptPhasePreEdit}, nil
	}
	if tabID == "" || paneID == "" || session == "" {
		return receiptLaunchEvidence{}, fmt.Errorf("partial post-launch identity")
	}
	workspace, err := workspaceFromHerdrIDs(tabID, paneID)
	if err != nil {
		return receiptLaunchEvidence{}, err
	}
	return receiptLaunchEvidence{receipt: receipt, phase: receiptPhasePostLaunch, workspace: workspace}, nil
}

func workspaceFromHerdrIDs(tabID, paneID string) (string, error) {
	tabWorkspace, tabResource, tabOK := strings.Cut(strings.TrimSpace(tabID), ":")
	paneWorkspace, paneResource, paneOK := strings.Cut(strings.TrimSpace(paneID), ":")
	if !tabOK || !paneOK || strings.TrimSpace(tabWorkspace) == "" || strings.TrimSpace(tabResource) == "" ||
		strings.TrimSpace(paneWorkspace) == "" || strings.TrimSpace(paneResource) == "" || tabWorkspace != paneWorkspace {
		return "", fmt.Errorf("post-launch identity has incomplete or conflicting Herdr workspace")
	}
	return tabWorkspace, nil
}

func exactHerdrAgentIdentity(agent herdr.AgentEntry) (string, error) {
	name := strings.TrimSpace(agent.Name)
	workspace := strings.TrimSpace(agent.Workspace)
	tabID := strings.TrimSpace(agent.TabID)
	paneID := strings.TrimSpace(agent.PaneID)
	session := strings.TrimSpace(agent.Session.Value)
	cwd := strings.TrimSpace(agent.Cwd)
	if name == "" || workspace == "" || tabID == "" || paneID == "" || session == "" || cwd == "" {
		return "", fmt.Errorf("dispatch: matching Herdr agent has incomplete live identity")
	}
	qualifiedWorkspace, err := workspaceFromHerdrIDs(tabID, paneID)
	if err != nil || qualifiedWorkspace != workspace {
		return "", fmt.Errorf("dispatch: matching Herdr agent has ambiguous workspace identity")
	}
	return strings.Join([]string{name, workspace, tabID, paneID, session, filepath.Clean(cwd)}, "\x00"), nil
}

func postLaunchIdentityMatches(candidate receiptLaunchEvidence, agent herdr.AgentEntry) bool {
	receipt := candidate.receipt
	return candidate.phase == receiptPhasePostLaunch &&
		strings.TrimSpace(agent.Name) == strings.TrimSpace(receipt.Name) &&
		sameWorktree(agent.Cwd, receipt.CWD) &&
		strings.TrimSpace(agent.Workspace) == candidate.workspace &&
		strings.TrimSpace(agent.TabID) == strings.TrimSpace(receipt.TabID) &&
		strings.TrimSpace(agent.PaneID) == strings.TrimSpace(receipt.PaneID) &&
		strings.TrimSpace(agent.Session.Value) == strings.TrimSpace(receipt.HerdrSession)
}

func sameWorktree(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}

func readLaunchReceiptsStrict(path string) ([]launch.Receipt, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	var receipts []launch.Receipt
	scanner := bufio.NewScanner(fh)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var receipt launch.Receipt
		if err := json.Unmarshal([]byte(line), &receipt); err != nil {
			return nil, fmt.Errorf("dispatch: launch receipt evidence is malformed: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return receipts, nil
}

// StaleRunRecovery is the coordinator-only composition root for exact local
// dispatch run recovery. Runstate owns the row CAS; dispatch owns the live
// lease and launch refusal authorities.
type StaleRunRecovery struct {
	Runs         *runstate.Store
	Tasks        provider.TaskProvider
	ProjectID    string
	Graph        runstate.GraphAuthority
	GraphForTask runstate.GraphAuthorityForTask
	Claims       security.LiveClaimLookup
	Launches     LiveLaunchLookup
}

func (r StaleRunRecovery) Recover(ctx context.Context, taskID, taskRef string) (*runstate.RunState, error) {
	if r.Runs == nil || r.Tasks == nil || strings.TrimSpace(r.ProjectID) == "" || r.Claims == nil || r.Launches == nil {
		return nil, fmt.Errorf("%w: incomplete recovery authorities", runstate.ErrAmbiguous)
	}
	req := runstate.RecoveryRequest{RunID: "dispatch:" + strings.TrimSpace(taskID), TaskID: strings.TrimSpace(taskID), TaskRef: strings.TrimSpace(taskRef), ProjectID: strings.TrimSpace(r.ProjectID)}
	authority := runstate.RecoveryAuthority{
		Authority: runstate.Authority{Tasks: r.Tasks, Graph: r.Graph, GraphForTask: r.GraphForTask},
		Guard: func(ctx context.Context, task runstate.TaskState) error {
			claim, err := r.Claims.LookupActiveClaim(ctx, task.Ref)
			if err == nil && claim != nil {
				return fmt.Errorf("%w: active lease generation %d for %s", ErrRecoveryActive, claim.Generation, task.Ref)
			}
			if err != nil && !errors.Is(err, security.ErrLeaseNotLive) {
				return fmt.Errorf("dispatch: stale run recovery lease state UNKNOWN: %w", err)
			}
			live, err := r.Launches.HasLiveLaunch(ctx, task.ID, task.Ref)
			if err != nil {
				return fmt.Errorf("dispatch: stale run recovery launch state UNKNOWN: %w", err)
			}
			if live {
				return fmt.Errorf("%w: admitted launch for %s", ErrRecoveryActive, task.Ref)
			}
			return nil
		},
	}
	return r.Runs.RecoverStale(ctx, req, authority)
}
