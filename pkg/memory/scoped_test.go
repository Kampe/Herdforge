package memory

import (
	"errors"
	"testing"
	"time"
)

func scopedFixture(t *testing.T) (*ScopedMemoryStore, Scope, Actor, time.Time) {
	t.Helper()
	store, err := NewScopedMemoryStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "worker-a", Role: "worker", Authenticated: true}
	scope := Scope{Kind: ScopeTask, RunID: "run-1", TaskID: "FAC-239", Role: "worker", Revision: "graph-r1", Readers: []string{"worker-a"}, Writers: []string{"worker-a"}}
	if err := store.RegisterScope(scope); err != nil {
		t.Fatal(err)
	}
	return store, scope, actor, time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
}

func TestScopedMemory_CrossTaskDeniedAndAuthorizedPacketInjection(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "Use revision fences", SourceEvidence: "receipt:task-run", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Promote(PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "reviewer-1", Role: "reviewer", Authenticated: true}, Revision: scope.Revision, TaskEvidence: "task:FAC-239", PromotedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	denied, err := store.Inject(ReadRequest{Actor: worker, RunID: "run-1", TaskID: "FAC-240", Role: "worker", Revision: scope.Revision, ReadAt: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 1 {
		t.Fatalf("other task received task-scoped proposal without an authorized relation: %+v", denied)
	}
	// The task proposal itself never crosses a task boundary; only its reviewed
	// global promotion may be injected.
	if denied[0].ProposalID != p.ID {
		t.Fatalf("unexpected knowledge: %+v", denied[0])
	}
}

func TestScopedMemory_StaleRevisionAndUnauthorizedPromotionRejected(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "proof", SourceEvidence: "receipt", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Promote(PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "worker-b", Role: "worker", Authenticated: true}, Revision: scope.Revision, TaskEvidence: "task", PromotedAt: now}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized promotion rejection, got %v", err)
	}
	if _, err := store.Promote(PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "reviewer", Role: "reviewer", Authenticated: true}, Revision: "graph-r2", TaskEvidence: "task", PromotedAt: now}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("expected stale promotion rejection, got %v", err)
	}
	if _, err := store.Inject(ReadRequest{Actor: worker, RunID: "run-1", TaskID: scope.TaskID, Role: scope.Role, Revision: "graph-r2", ReadAt: now}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("expected stale packet rejection, got %v", err)
	}
}

func TestScopedMemory_PromotionIsIdempotentAndAuditable(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "bounded context", SourceEvidence: "receipt:source", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	req := PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "coordinator", Role: "coordinator", Authenticated: true}, Revision: scope.Revision, TaskEvidence: "task:FAC-239", PromotedAt: now.Add(time.Minute)}
	first, err := store.Promote(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Promote(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("promotion was not idempotent: %q != %q", first.ID, second.ID)
	}
	if _, err := store.Inject(ReadRequest{Actor: worker, RunID: scope.RunID, TaskID: scope.TaskID, Role: scope.Role, Revision: scope.Revision, ReadAt: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 3 || evidence[0].Action != "promotion" || evidence[0].TaskID != scope.TaskID || evidence[0].Revision != scope.Revision || evidence[1].Action != "read" || evidence[2].Action != "read" {
		t.Fatalf("missing durable promotion/read evidence: %+v", evidence)
	}
}

func TestScopedMemory_ExpiredEvidenceCannotBePromoted(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "expired", SourceEvidence: "receipt", CreatedAt: now, ExpiresAt: now.Add(time.Minute), RetainUntil: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Promote(PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "reviewer", Role: "reviewer", Authenticated: true}, Revision: scope.Revision, TaskEvidence: "task", PromotedAt: now.Add(time.Minute)})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expired evidence rejection, got %v", err)
	}
}

func TestScopedMemory_ReopenRetainsPromotionIdempotence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewScopedMemoryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "worker", Role: "worker", Authenticated: true}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	scope := Scope{Kind: ScopeTask, RunID: "run", TaskID: "FAC-239", Role: "worker", Revision: "r1", Readers: []string{"worker"}, Writers: []string{"worker"}}
	if err := store.RegisterScope(scope); err != nil {
		t.Fatal(err)
	}
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: actor, Content: "durable", SourceEvidence: "receipt", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	req := PromotionRequest{ProposalID: p.ID, Actor: Actor{ID: "reviewer", Role: "reviewer", Authenticated: true}, Revision: "r1", TaskEvidence: "task", PromotedAt: now.Add(time.Minute)}
	first, err := store.Promote(req)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewScopedMemoryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reopened.Promote(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("promotion lost idempotence after reopen: %q != %q", first.ID, second.ID)
	}
}

func TestScopedMemory_DeniesCrossRunAndRoleMismatch(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "run-one-only", SourceEvidence: "receipt", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Inject(ReadRequest{Actor: worker, RunID: "run-2", TaskID: scope.TaskID, Role: scope.Role, Revision: scope.Revision, ReadAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cross-run read leaked proposal %s: %+v", p.ID, entries)
	}
	roleScope := Scope{Kind: ScopeRole, RunID: scope.RunID, TaskID: scope.TaskID, Role: "reviewer", Revision: scope.Revision, Readers: []string{worker.ID}}
	if scopeMatchesRequest(roleScope, ReadRequest{RunID: scope.RunID, TaskID: scope.TaskID, Role: "worker"}) {
		t.Fatal("role scope matched a different role")
	}
}

func TestScopedMemory_RelationRequiresExactRevision(t *testing.T) {
	store, scope, worker, now := scopedFixture(t)
	p, err := store.Propose(ProposalRequest{Scope: scope, Actor: worker, Content: "related-task-only", SourceEvidence: "receipt", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuthorizeRelation(ScopeRelation{FromTask: "FAC-240", ToTask: scope.TaskID, Revision: "other-revision"}); err != nil {
		t.Fatal(err)
	}
	request := ReadRequest{Actor: worker, RunID: scope.RunID, TaskID: "FAC-240", Role: scope.Role, Revision: scope.Revision, ReadAt: now.Add(time.Minute)}
	entries, err := store.Inject(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("relation with different revision leaked proposal %s: %+v", p.ID, entries)
	}
	if err := store.AuthorizeRelation(ScopeRelation{FromTask: "FAC-240", ToTask: scope.TaskID, Revision: scope.Revision}); err != nil {
		t.Fatal(err)
	}
	entries, err = store.Inject(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != p.ID {
		t.Fatalf("same-revision relation did not authorize exact proposal: %+v", entries)
	}
}
