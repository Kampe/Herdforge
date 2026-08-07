package containerlifecycle

import (
	"context"
	"errors"
	"testing"
)

func fakeLister(rows []LiveContainer, err error) ContainerLister {
	return func(context.Context) ([]LiveContainer, error) { return rows, err }
}

// TestAuditUnownedMatchesFullNotTruncatedIDs guards against the class of
// bug where a live-listing source returns Docker's default truncated
// (12-char) ID while receipts are registered under the full 64-char ID
// `docker create`/`docker inspect` return: a naive audit would then
// report every owned container as unowned. DockerListAll itself must
// pass --no-trunc (see docker_test.go); this proves AuditUnowned's own
// matching logic correctly recognizes a full-length ID as owned when the
// lister returns full IDs.
func TestAuditUnownedMatchesFullNotTruncatedIDs(t *testing.T) {
	s := newTestStore(t)
	fullID := "18381a40355cf1333b45c51862b1d3ad16976126a233c8fbcba107571a267ef6"
	if _, err := s.Register(Receipt{ContainerID: fullID, TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := fakeLister([]LiveContainer{{ID: fullID, Image: "fac200:v1", Status: "Up"}}, nil)
	unowned, err := AuditUnowned(context.Background(), s, list)
	if err != nil {
		t.Fatalf("AuditUnowned: %v", err)
	}
	if len(unowned) != 0 {
		t.Fatalf("unowned = %+v, want the owned full-ID container excluded", unowned)
	}
	// A truncated form of the same container must NOT be treated as a
	// match for the full-ID receipt — that would be the same class of
	// bug in the opposite direction (silently crediting ownership to the
	// wrong identity string).
	truncated := fullID[:12]
	listTruncated := fakeLister([]LiveContainer{{ID: truncated, Image: "fac200:v1", Status: "Up"}}, nil)
	unownedTruncated, err := AuditUnowned(context.Background(), s, listTruncated)
	if err != nil {
		t.Fatalf("AuditUnowned: %v", err)
	}
	if len(unownedTruncated) != 1 {
		t.Fatalf("a truncated id must not match the full-id receipt by exact string equality: unowned=%+v", unownedTruncated)
	}
}

func TestAuditUnownedReportsOnlyContainersWithoutAReceipt(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "owned1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := fakeLister([]LiveContainer{
		{ID: "owned1", Image: "fac200:v1", Status: "Up"},
		{ID: "legacy-fac174-a", Image: "fac174-hermetic:d0cc889", Status: "Exited (0)"},
		{ID: "legacy-fac174-b", Image: "fac174-hermetic:54ac3e2", Status: "Created"},
	}, nil)
	unowned, err := AuditUnowned(context.Background(), s, list)
	if err != nil {
		t.Fatalf("AuditUnowned: %v", err)
	}
	if len(unowned) != 2 {
		t.Fatalf("unowned = %+v, want 2", unowned)
	}
	ids := map[string]bool{unowned[0].ID: true, unowned[1].ID: true}
	if !ids["legacy-fac174-a"] || !ids["legacy-fac174-b"] {
		t.Fatalf("unowned ids = %v, want the two legacy containers", ids)
	}
	if ids["owned1"] {
		t.Fatal("AuditUnowned reported an owned container as unowned")
	}
}

func TestAuditUnownedNeverRemovesAnything(t *testing.T) {
	// AuditUnowned's signature has no Remover — this is enforced at
	// compile time, but assert the behavioral contract too: running it
	// twice against the same fixture yields the same report, proving it
	// has no side effect on the store or the (fake) host.
	s := newTestStore(t)
	list := fakeLister([]LiveContainer{{ID: "legacy", Image: "fac174-hermetic:x"}}, nil)
	first, err := AuditUnowned(context.Background(), s, list)
	if err != nil {
		t.Fatalf("first AuditUnowned: %v", err)
	}
	second, err := AuditUnowned(context.Background(), s, list)
	if err != nil {
		t.Fatalf("second AuditUnowned: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("first=%+v second=%+v, want identical repeated reports", first, second)
	}
}

func TestStatusCategorizesReceiptsByState(t *testing.T) {
	s := newTestStore(t)
	mustRegister := func(id string) {
		if _, err := s.Register(Receipt{ContainerID: id, TaskRef: "FAC-200", Generation: "g1"}); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}
	mustRegister("active")
	mustRegister("awaiting")
	if err := s.MarkAwaitingCleanup("awaiting", "success"); err != nil {
		t.Fatalf("MarkAwaitingCleanup: %v", err)
	}
	mustRegister("removed")
	if err := s.MarkRemoved("removed", true); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	mustRegister("quarantined")
	if err := s.MarkQuarantined("quarantined", "boom"); err != nil {
		t.Fatalf("MarkQuarantined: %v", err)
	}

	report, err := Status(context.Background(), s, fakeLister([]LiveContainer{{ID: "legacy"}}, nil))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(report.OwnedActive) != 1 || report.OwnedActive[0].ContainerID != "active" {
		t.Fatalf("OwnedActive = %+v", report.OwnedActive)
	}
	if len(report.OwnedAwaitingCleanup) != 1 || report.OwnedAwaitingCleanup[0].ContainerID != "awaiting" {
		t.Fatalf("OwnedAwaitingCleanup = %+v", report.OwnedAwaitingCleanup)
	}
	if len(report.Removed) != 1 || report.Removed[0].ContainerID != "removed" {
		t.Fatalf("Removed = %+v", report.Removed)
	}
	if len(report.Quarantined) != 1 || report.Quarantined[0].ContainerID != "quarantined" {
		t.Fatalf("Quarantined = %+v", report.Quarantined)
	}
	if len(report.Unowned) != 1 || report.Unowned[0].ID != "legacy" {
		t.Fatalf("Unowned = %+v", report.Unowned)
	}
}

func TestStatusSurvivesAuditFailureWithoutLosingReceiptData(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "active", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	report, err := Status(context.Background(), s, fakeLister(nil, errors.New("docker: command not found")))
	if err != nil {
		t.Fatalf("Status should not fail just because the live audit failed: %v", err)
	}
	if len(report.OwnedActive) != 1 {
		t.Fatalf("OwnedActive = %+v, want receipt data preserved", report.OwnedActive)
	}
	if report.AuditError == "" {
		t.Fatal("expected AuditError to be set")
	}
	if report.Unowned != nil {
		t.Fatalf("Unowned = %+v, want nil on audit failure", report.Unowned)
	}
}
