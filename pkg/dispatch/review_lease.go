package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/envelope"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/security"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

const coordinatorReviewOwner = "coordinator-review"

// CanonicalReviewLeaseTaskRef maps FAC-N or fac-N:review onto the scoped
// coordinator-review lease key used by requireLiveLease.
func CanonicalReviewLeaseTaskRef(ref string) string {
	ref = strings.ToLower(hsync.NormalizeRef(strings.TrimSpace(ref)))
	ref = strings.TrimSuffix(ref, ":review")
	return ref + ":review"
}

func isReviewerControlBind(taskRef, agentName string) bool {
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(taskRef)), ":review") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(agentName)), "review-")
}

func lookupActiveCoordinatorReviewLease(ctx context.Context, root, taskRef string) (*claim.Lease, error) {
	reviewRef := CanonicalReviewLeaseTaskRef(taskRef)
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: review lease store root required", security.ErrLeaseNotLive)
	}
	st, err := claim.NewSQLiteLeaseStore(lifecycle.CanonicalStatePath(root))
	if err != nil {
		return nil, fmt.Errorf("%w: open coordinator-review lease store: %v", security.ErrLeaseNotLive, err)
	}
	defer st.Close()
	now := time.Now()
	leases, err := st.ActiveClaims(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("%w: read coordinator-review leases: %v", security.ErrLeaseNotLive, err)
	}
	var found *claim.Lease
	for _, l := range leases {
		if l == nil || l.TaskRef != reviewRef {
			continue
		}
		if l.OwnerID != coordinatorReviewOwner {
			continue
		}
		if l.Status != claim.StatusActive || l.Expired(now) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: ambiguous coordinator-review leases for %s", security.ErrLeaseNotLive, reviewRef)
		}
		found = l
	}
	if found == nil {
		return nil, fmt.Errorf("%w: no active coordinator-review lease for %s", security.ErrLeaseNotLive, reviewRef)
	}
	return found, nil
}

func (c *ControlPlane) liveAgentLookup() security.LiveAgentResolver {
	if c != nil && c.AgentResolver != nil {
		return c.AgentResolver
	}
	return herdr.LiveResolver{}
}

func (c *ControlPlane) bindLiveAgentSession(workerHint, agentName string) (string, error) {
	if agentName != "" {
		live, herr := c.liveAgentLookup().Lookup(agentName)
		if herr != nil || live == nil || live.AgentSessionID == "" {
			return "", fmt.Errorf("%w: live AgentSessionID unresolved for %s: %v", envelope.ErrMissingBinding, agentName, herr)
		}
		if workerHint != "" && workerHint != live.AgentSessionID {
			return "", fmt.Errorf("%w: --worker %q does not match live AgentSessionID %q", envelope.ErrWorkerMismatch, workerHint, live.AgentSessionID)
		}
		return live.AgentSessionID, nil
	}
	if workerHint == "" {
		return "", fmt.Errorf("%w: --worker or --agent required to resolve live AgentSessionID", envelope.ErrMissingBinding)
	}
	if live, herr := c.liveAgentLookup().Lookup(workerHint); herr == nil && live != nil && live.AgentSessionID != "" {
		return live.AgentSessionID, nil
	}
	if c != nil && c.RequireLiveBind {
		return "", fmt.Errorf("%w: cannot resolve live AgentSessionID for worker %q", envelope.ErrMissingBinding, workerHint)
	}
	return workerHint, nil
}

func (c *ControlPlane) resolveReviewLeaseBinding(taskRef, workerHint string, leaseHint int64, agentName string) (string, int64, error) {
	lease, err := lookupActiveCoordinatorReviewLease(context.Background(), c.DurableRoot, taskRef)
	if err != nil {
		return "", 0, err
	}
	if leaseHint > 0 && leaseHint != lease.Generation {
		return "", 0, fmt.Errorf("%w: --lease %d does not match live review generation %d", security.ErrLeaseNotLive, leaseHint, lease.Generation)
	}
	slug := strings.TrimSuffix(CanonicalReviewLeaseTaskRef(taskRef), ":review")
	if agentName != "" && !strings.Contains(strings.ToLower(agentName), slug) {
		return "", 0, fmt.Errorf("%w: reviewer agent %q is not bound to %s", security.ErrLeaseNotLive, agentName, slug)
	}
	ws, err := c.bindLiveAgentSession(workerHint, agentName)
	if err != nil {
		return "", 0, err
	}
	return ws, lease.Generation, nil
}
