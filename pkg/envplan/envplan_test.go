package envplan

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func binding() Binding {
	return Binding{TaskRef: "FAC-241", TaskID: "t-1", Provider: "memory", ProviderRevision: "p1", GraphRevision: "g1", RunID: "dispatch:t-1", RunRevision: 1}
}
func request(c Capability) Request {
	return Request{Capability: c, Evidence: Evidence{Authority: "security", Revision: "r1", Subject: "lane:worker"}}
}

func TestPlanRejectsUnplannedAndStaleActionsBeforeTheyCanRun(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	p, err := s.Create(context.Background(), Plan{Binding: binding(), Requests: []Request{request(CapabilityNetwork)}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(context.Background(), p.ID, binding(), CapabilityBoardWrite, now); !errors.Is(err, ErrUnplanned) {
		t.Fatalf("unplanned action=%v", err)
	}
	if _, err := s.Grant(context.Background(), p.ID, CapabilityNetwork, "operator", now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	drift := binding()
	drift.GraphRevision = "g2"
	if err := s.Authorize(context.Background(), p.ID, drift, CapabilityNetwork, now); !errors.Is(err, ErrStale) {
		t.Fatalf("stale binding=%v", err)
	}
	if err := s.Authorize(context.Background(), p.ID, binding(), CapabilityNetwork, now); err != nil {
		t.Fatalf("granted planned action=%v", err)
	}
}

func TestNoCapabilitiesCreatesNoApprovalPrompt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	p, err := s.Create(context.Background(), Plan{Binding: binding(), CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if p.NeedsApproval() || len(p.Grants) != 0 {
		t.Fatalf("empty plan unexpectedly asks for approval: %+v", p)
	}
	if _, err := s.Grant(context.Background(), p.ID, CapabilityNetwork, "operator", now.Add(time.Hour)); !errors.Is(err, ErrUnplanned) {
		t.Fatalf("empty plan grant=%v", err)
	}
}

func TestGrantIsIdempotentAndNeverStoresCredentials(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	p, err := s.Create(context.Background(), Plan{Binding: binding(), Requests: []Request{request(CapabilityCredential)}, CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	first := now.Add(time.Hour)
	if _, err = s.Grant(context.Background(), p.ID, CapabilityCredential, "operator", first); err != nil {
		t.Fatal(err)
	}
	p, err = s.Grant(context.Background(), p.ID, CapabilityCredential, "operator", first)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Grants) != 1 || !p.Grants[0].ExpiresAt.Equal(first) {
		t.Fatalf("grant not idempotent: %+v", p.Grants)
	}
	raw := p.Grants[0]
	if raw.Operator != "operator" || raw.Capability != CapabilityCredential {
		t.Fatalf("unexpected grant material: %+v", raw)
	}
}
