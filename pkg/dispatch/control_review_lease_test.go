package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/envelope"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/security"
)

type staticAgentResolver map[string]*security.LiveAgentIdentity

func (m staticAgentResolver) Lookup(name string) (*security.LiveAgentIdentity, error) {
	if rec, ok := m[name]; ok && rec != nil {
		cp := *rec
		return &cp, nil
	}
	return nil, security.ErrAgentNotFound
}

func (staticAgentResolver) CloseTab(string) error { return nil }

func seedCoordinatorReviewLease(t *testing.T, root, taskRef string, genWant int64, ttl time.Duration) *claim.Lease {
	t.Helper()
	path := filepath.Join(root, ".herd", "herdforge.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := claim.NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := claim.LeaseKey{Repo: "herdforge", Provider: "kaneo", Project: "proj", TaskRef: taskRef}
	lease, err := st.Acquire(context.Background(), key, "coordinator-review", RoleReviewer, "", time.Now(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	if genWant > 0 && lease.Generation != genWant {
		t.Fatalf("seed generation=%d want %d", lease.Generation, genWant)
	}
	return lease
}

func reviewBindPlane(t *testing.T, root string, agents staticAgentResolver) *ControlPlane {
	t.Helper()
	return &ControlPlane{
		Secret:          ctrlSecret,
		Mailbox:         mail.NewMailbox(filepath.Join(root, "mail.jsonl")),
		IssuerRole:      envelope.RoleCoordinator,
		IssuerSession:   "coordinator",
		DurableRoot:     root,
		ClaimLookup:     security.MapClaimLookup{}, // no claimed_tasks row
		AgentResolver:   agents,
		RequireLiveBind: true,
	}
}

// TestResolveLiveControlBinding_ReviewLeaseWithoutWorkerClaim is the live
// FAC-739 canary: reviewer drain must bind the exact coordinator-review
// lease in shared herdforge.db when claimed_tasks has no FAC-N row.
func TestResolveLiveControlBinding_ReviewLeaseWithoutWorkerClaim(t *testing.T) {
	root := t.TempDir()
	lease := seedCoordinatorReviewLease(t, root, "fac-737:review", 1, time.Hour)
	agent := "review-fac-737-aac78129e970"
	session := "sess-review-737"
	cp := reviewBindPlane(t, root, staticAgentResolver{
		agent: {Name: agent, AgentSessionID: session},
	})

	ws, gen, err := cp.ResolveLiveControlBinding("FAC-737", "", 0, agent)
	if err != nil {
		t.Fatalf("reviewer drain must bind coordinator-review lease without a worker claim: %v", err)
	}
	if gen != lease.Generation {
		t.Fatalf("lease generation=%d want %d", gen, lease.Generation)
	}
	if ws != session {
		t.Fatalf("session=%q want %q", ws, session)
	}

	ws2, gen2, err := cp.ResolveLiveControlBinding("fac-737:review", "", 0, agent)
	if err != nil {
		t.Fatalf("fac-N:review must canonicalize to the same review lease: %v", err)
	}
	if ws2 != ws || gen2 != gen {
		t.Fatalf("canonicalization diverged: %s/%d vs %s/%d", ws2, gen2, ws, gen)
	}

	scope := &envelope.Scope{Exclusive: true, Note: "review-bind"}
	ctrl, _, err := cp.IssueAndPostScope(ws, "FAC-737", gen, scope, "retain signed reviewer envelope")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	sess, applied, err := cp.ApplyInboxControl(ws, "FAC-737", gen)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if len(applied) != 1 || applied[0].Err != nil || applied[0].Decision == nil || applied[0].Decision.Status != envelope.StatusApplied {
		t.Fatalf("signed reviewer envelope must bind and retain: %+v", applied)
	}
	if sess.LastSequence() != ctrl.Sequence {
		t.Fatalf("retained seq=%d want %d", sess.LastSequence(), ctrl.Sequence)
	}
}

func TestResolveLiveControlBinding_ReviewLeaseRefusals(t *testing.T) {
	root := t.TempDir()
	lease := seedCoordinatorReviewLease(t, root, "fac-737:review", 1, time.Hour)
	agent := "review-fac-737-aac78129e970"
	session := "sess-review-737"
	agents := staticAgentResolver{
		agent:                    {Name: agent, AgentSessionID: session},
		"review-fac-738-other":   {Name: "review-fac-738-other", AgentSessionID: "sess-738"},
		"review-fac-737-staleid": {Name: "review-fac-737-staleid", AgentSessionID: "other-session"},
	}
	cp := reviewBindPlane(t, root, agents)

	t.Run("wrong generation", func(t *testing.T) {
		_, _, err := cp.ResolveLiveControlBinding("FAC-737", "", lease.Generation+1, agent)
		if !errors.Is(err, security.ErrLeaseNotLive) {
			t.Fatalf("want ErrLeaseNotLive, got %v", err)
		}
	})
	t.Run("wrong task", func(t *testing.T) {
		_, _, err := cp.ResolveLiveControlBinding("FAC-738", "", 0, "review-fac-738-other")
		if !errors.Is(err, security.ErrLeaseNotLive) {
			t.Fatalf("want ErrLeaseNotLive for missing review lease, got %v", err)
		}
	})
	t.Run("wrong agent", func(t *testing.T) {
		_, _, err := cp.ResolveLiveControlBinding("FAC-737", "", 0, "review-fac-738-other")
		if err == nil {
			t.Fatal("wrong reviewer agent must not bind")
		}
	})
	t.Run("wrong session", func(t *testing.T) {
		_, _, err := cp.ResolveLiveControlBinding("FAC-737", "invented-session", 0, agent)
		if !errors.Is(err, envelope.ErrWorkerMismatch) {
			t.Fatalf("want ErrWorkerMismatch, got %v", err)
		}
	})
	t.Run("stale released lease", func(t *testing.T) {
		staleRoot := t.TempDir()
		stale := seedCoordinatorReviewLease(t, staleRoot, "fac-737:review", 1, time.Hour)
		st, err := claim.NewSQLiteLeaseStore(filepath.Join(staleRoot, ".herd", "herdforge.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.Release(context.Background(), stale.LeaseKey, stale.OwnerID, stale.Generation, time.Now()); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		staleCP := reviewBindPlane(t, staleRoot, agents)
		_, _, err = staleCP.ResolveLiveControlBinding("FAC-737", "", 0, agent)
		if !errors.Is(err, security.ErrLeaseNotLive) {
			t.Fatalf("released review lease must refuse, got %v", err)
		}
	})
}

func TestResolveLiveControlBinding_WorkerClaimPathUnchanged(t *testing.T) {
	root := t.TempDir()
	_ = seedCoordinatorReviewLease(t, root, "fac-737:review", 1, time.Hour)
	worker := "worker-fac-737"
	session := "sess-worker-737"
	cp := reviewBindPlane(t, root, staticAgentResolver{
		worker: {Name: worker, AgentSessionID: session},
	})
	cp.ClaimLookup = security.MapClaimLookup{
		"FAC-737": {TaskRef: "FAC-737", Generation: 9, OwnerID: "worker", Role: RoleWorker, ExpiresAt: time.Now().Add(time.Hour)},
	}
	ws, gen, err := cp.ResolveLiveControlBinding("FAC-737", "", 0, worker)
	if err != nil {
		t.Fatalf("worker path: %v", err)
	}
	if gen != 9 {
		t.Fatalf("worker must keep FAC-147 claim generation, got %d", gen)
	}
	if ws != session {
		t.Fatalf("worker session=%q want %q", ws, session)
	}
	if strings.Contains(ws, "review") {
		t.Fatal("worker bind must not use the review session")
	}
}
