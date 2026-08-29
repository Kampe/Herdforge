package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	candidates := make([]launch.Receipt, 0)
	for _, receipt := range receipts {
		if !receipt.Accepted || receipt.TaskRef != taskRef {
			continue
		}
		if receipt.Name == "" || receipt.TabID == "" || receipt.PaneID == "" {
			return false, fmt.Errorf("dispatch: accepted launch receipt for %s has incomplete identity", taskRef)
		}
		candidates = append(candidates, receipt)
	}
	if len(candidates) == 0 {
		return false, nil
	}
	agents, err := l.list()
	if err != nil {
		return false, err
	}
	for _, receipt := range candidates {
		for _, agent := range agents {
			if agent.Name == receipt.Name && agent.TabID == receipt.TabID && agent.PaneID == receipt.PaneID && (receipt.HerdrSession == "" || agent.Session.Value == "" || agent.Session.Value == receipt.HerdrSession) {
				return true, nil
			}
		}
	}
	return false, nil
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
