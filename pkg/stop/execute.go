package stop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Fleet is the live boundary an execution drives; in production every method
// is a herdr call. Note what is absent: there is no way to remove a worktree,
// delete a branch, or drop a ref. Stop releases terminal capacity and nothing
// else, so no flag — including --force-working — can reach unique work.
type Fleet interface {
	// Agents re-reads the fleet. Stop calls it after signaling so a close is
	// decided on live state, not on the plan's stale snapshot.
	Agents() ([]Agent, error)
	RequestStop(paneID string) error
	CloseTab(tabID string) error
}

// Exec runs a plan. Execute=false is the default dry run: it touches neither
// the durable posture nor the fleet.
type Exec struct {
	Fleet Fleet
	// EnterWinddown must return nil only once "no new work" is durable. It is
	// called before the first worker is signaled and must be idempotent, so a
	// stop interrupted midway resumes into the same posture on restart.
	EnterWinddown func(context.Context) error
	Out           io.Writer
	Execute       bool
	ForceWorking  bool
	// Wait bounds how long settling workers are given to reach a checkpoint.
	// Zero still performs one just-in-time re-read before any close.
	Wait time.Duration
	Poll time.Duration
}

// Result counts one execution.
type Result struct {
	Closed    int // tabs closed; capacity released
	Preserved int // left open and recoverable
	Protected int // coordinator skipped
	Signaled  int // stop requests proven delivered
	Unproven  int // stop requests that failed; never closed without force
}

const defaultPoll = 2 * time.Second

// Run executes plan against the fleet.
//
// Ordering is the safety property: wind-down becomes durable first (otherwise
// the kick loop re-claims a worker while it is settling), then workers are
// signaled, then a bounded wait lets them checkpoint, and only a worker
// observed settled on a fresh read has its tab closed. Capacity is released
// exactly once per tab.
func (e Exec) Run(ctx context.Context, plan []Decision) (Result, error) {
	out := e.Out
	if out == nil {
		out = io.Discard
	}
	if !e.Execute {
		return dryRun(out, plan), nil
	}
	if e.Fleet == nil || e.EnterWinddown == nil {
		return Result{}, errors.New("stop: execute requires a fleet and a wind-down authority")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := e.EnterWinddown(ctx); err != nil {
		return Result{}, fmt.Errorf("wind-down not durable; no worker signaled: %w", err)
	}
	fmt.Fprintln(out, "WINDDOWN durable=true")

	var res Result
	unproven := map[string]bool{}
	for _, d := range plan {
		if d.Action == Protect {
			res.Protected++
			fmt.Fprintf(out, "PROTECT name=%s status=%s tab=%s (%s)\n", d.Agent.Name, d.Agent.Status, d.Agent.TabID, d.Reason)
			continue
		}
		if !d.RequestStop || d.Agent.PaneID == "" {
			continue
		}
		if err := e.Fleet.RequestStop(d.Agent.PaneID); err != nil {
			unproven[d.Agent.Name] = true
			res.Unproven++
			fmt.Fprintf(out, "UNPROVEN name=%s pane=%s (%v)\n", d.Agent.Name, d.Agent.PaneID, err)
			continue
		}
		res.Signaled++
		fmt.Fprintf(out, "STOP_REQUESTED name=%s pane=%s\n", d.Agent.Name, d.Agent.PaneID)
	}

	live, waitErr := e.settle(ctx, out, plan)
	if waitErr != nil {
		// A cancelled or unreadable wait leaves every worker open and
		// recoverable; the durable posture keeps the fleet quiet until a
		// restart resumes this same state machine.
		fmt.Fprintf(out, "ABORTED wait=%v (%v)\n", e.Wait, waitErr)
		for _, d := range plan {
			if d.Action != Protect {
				res.Preserved++
			}
		}
		return res, waitErr
	}

	closed := map[string]bool{}
	for _, d := range plan {
		if d.Action == Protect {
			continue
		}
		status, known := live[d.Agent.Name]
		switch {
		case e.ForceWorking:
			// Explicit operator force: close regardless of settlement. Still
			// only a tab close — the worktree, branch, and refs survive.
		case known && IsActive(status):
			res.Preserved++
			fmt.Fprintf(out, "PRESERVE name=%s status=%s pane=%s (never checkpointed; source work not destroyed)\n", d.Agent.Name, status, d.Agent.PaneID)
			continue
		case unproven[d.Agent.Name]:
			res.Preserved++
			fmt.Fprintf(out, "PRESERVE name=%s status=%s pane=%s (stop request unproven; ownership not checkpointed)\n", d.Agent.Name, status, d.Agent.PaneID)
			continue
		}
		if d.Agent.TabID == "" || closed[d.Agent.TabID] {
			continue
		}
		closed[d.Agent.TabID] = true
		if err := e.Fleet.CloseTab(d.Agent.TabID); err != nil {
			res.Preserved++
			fmt.Fprintf(out, "PRESERVE name=%s tab=%s (close failed: %v)\n", d.Agent.Name, d.Agent.TabID, err)
			continue
		}
		res.Closed++
		fmt.Fprintf(out, "CLOSE name=%s status=%s tab=%s\n", d.Agent.Name, status, d.Agent.TabID)
	}
	return res, nil
}

// settle re-reads the fleet until every signaled worker has left an active
// state or the bounded wait expires. It always reads at least once, so a
// worker that turned active between planning and closing is caught even at
// Wait=0.
func (e Exec) settle(ctx context.Context, out io.Writer, plan []Decision) (map[string]string, error) {
	poll := e.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	deadline := time.Now().Add(e.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		agents, err := e.Fleet.Agents()
		if err != nil {
			return nil, fmt.Errorf("re-read fleet: %w", err)
		}
		live := make(map[string]string, len(agents))
		for _, a := range agents {
			live[a.Name] = a.Status
		}
		pending := pendingNames(plan, live)
		if len(pending) == 0 || !time.Now().Before(deadline) {
			return live, nil
		}
		fmt.Fprintf(out, "WAITING pending=%d agents=%v\n", len(pending), pending)
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// pendingNames lists planned agents still active on the live read.
func pendingNames(plan []Decision, live map[string]string) []string {
	var pending []string
	for _, d := range plan {
		if d.Action == Protect {
			continue
		}
		if status, ok := live[d.Agent.Name]; ok && IsActive(status) {
			pending = append(pending, d.Agent.Name)
		}
	}
	return pending
}

func dryRun(out io.Writer, plan []Decision) Result {
	var res Result
	for _, d := range plan {
		switch d.Action {
		case Protect:
			res.Protected++
			fmt.Fprintf(out, "PROTECT name=%s status=%s tab=%s (%s)\n", d.Agent.Name, d.Agent.Status, d.Agent.TabID, d.Reason)
		case Preserve:
			res.Preserved++
			fmt.Fprintf(out, "WOULD_PRESERVE name=%s status=%s pane=%s (%s)\n", d.Agent.Name, d.Agent.Status, d.Agent.PaneID, d.Reason)
		default:
			res.Closed++
			fmt.Fprintf(out, "WOULD_CLOSE name=%s status=%s tab=%s\n", d.Agent.Name, d.Agent.Status, d.Agent.TabID)
		}
	}
	return res
}
