package remoteci

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/recovery"
)

// FailureRouter is deliberately narrow: remote CI may report a terminal
// failure, but it cannot dispatch, verify, review, or merge a replacement.
type FailureRouter interface {
	RouteTerminalFailure(context.Context, Settlement) error
}

// PersistAndRouteTerminal stores a terminal result first. The router is called
// only for the first durable failed settlement, which makes recovery routing
// exactly once even after poller retries or process crashes.
func PersistAndRouteTerminal(ctx context.Context, store *Store, settlement Settlement, router FailureRouter) error {
	written, err := store.PersistTerminal(settlement)
	if err != nil || !written || settlement.State != StateFailed {
		return err
	}
	if router == nil {
		return fmt.Errorf("%w: no recovery router for terminal failure", ErrInvalid)
	}
	return router.RouteTerminalFailure(ctx, settlement)
}

// RecoveryRouter adapts the FAC-236 durable recovery DAG; it records a bounded
// retry-new-approach decision and has no authority to bypass local gates.
type RecoveryRouter struct {
	Store    *recovery.Store
	Run      string
	Task     string
	Actor    string
	Revision int64
	Graph    string
}

func (r RecoveryRouter) RouteTerminalFailure(_ context.Context, settlement Settlement) error {
	if r.Store == nil {
		return fmt.Errorf("remote-ci: recovery store is required")
	}
	return r.Store.Decide(recovery.Decision{
		Run: r.Run, Task: r.Task, Actor: r.Actor,
		Evidence:    "remote-ci:" + watchKey(settlement.Binding),
		Disposition: recovery.RetryNewApproach, Revision: r.Revision, Graph: r.Graph,
	})
}
