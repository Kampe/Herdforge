package deps

import (
	"context"
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestArchivedPrerequisiteIsTerminalNotOpen reproduces FAC-777: an authenticated
// archived prerequisite can never become "done", so treating it as an open
// blocker poisons every downstream launch permanently. Archived is a terminal
// outcome (same closure family as done) and must satisfy the prerequisite.
func TestArchivedPrerequisiteIsTerminalNotOpen(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("CHA-2554", "archived", provider.PriorityHigh)
	m.EnsureTask("FAC-777", "to-do", provider.PriorityHigh)
	if _, err := m.SeedBlocks("CHA-2554", "FAC-777"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	des := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-777", TaskID: "id-fac-777", Present: true,
		Edges: []DependencyEdge{{SourceRef: "CHA-2554", TargetRef: "FAC-777", Type: EdgeBlocks}},
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-777", des, "")
	if err != nil {
		var blocked *BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("archived prerequisite must not poison launch (terminal, not open): code=%s reason=%s", blocked.Code, blocked.Reason)
		}
		t.Fatalf("archived prerequisite must not poison launch: %v", err)
	}
}

// TestArchivedTaskItselfIsIneligible: an archived node remains terminal but is
// itself never dispatchable, same as done.
func TestArchivedTaskItselfIsIneligible(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-777", "archived", provider.PriorityHigh)
	des := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-777", TaskID: "id-fac-777", Present: true,
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-777", des, "")
	var blocked *BlockedError
	if err == nil || !errors.As(err, &blocked) || blocked.Code != "terminal" {
		t.Fatalf("archived launch target must be BLOCKED(terminal), got %v", err)
	}
}

// TestDoneTaskItselfIsIneligible: regression control -- done must stay blocked
// the same way, unchanged by the archived fix.
func TestDoneTaskItselfIsIneligible(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-777", "done", provider.PriorityHigh)
	des := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-777", TaskID: "id-fac-777", Present: true,
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-777", des, "")
	var blocked *BlockedError
	if err == nil || !errors.As(err, &blocked) || blocked.Code != "terminal" {
		t.Fatalf("done launch target must be BLOCKED(terminal), got %v", err)
	}
}

// TestOpenPrerequisiteStillBlocks: negative control -- a real live (non-terminal)
// prerequisite must keep blocking. Must go RED if the archived-terminal fix is
// broadened past done/archived.
func TestOpenPrerequisiteStillBlocks(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-1", "to-do", provider.PriorityHigh)
	m.EnsureTask("FAC-777", "to-do", provider.PriorityHigh)
	if _, err := m.SeedBlocks("FAC-1", "FAC-777"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	des := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-777", TaskID: "id-fac-777", Present: true,
		Edges: []DependencyEdge{{SourceRef: "FAC-1", TargetRef: "FAC-777", Type: EdgeBlocks}},
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-777", des, "")
	var blocked *BlockedError
	if err == nil || !errors.As(err, &blocked) || blocked.Code != "open_blocker" {
		t.Fatalf("open (to-do) prerequisite must BLOCK(open_blocker), got %v", err)
	}
}

// TestPrerequisiteUnreadableStaysBlocked: negative control -- a prerequisite
// that errors on TaskStatus (deleted/forbidden/unresolved) must stay fail-closed,
// never silently treated as archived/satisfied.
func TestPrerequisiteUnreadableStaysBlocked(t *testing.T) {
	m := NewMemoryStore()
	m.EnsureTask("FAC-777", "to-do", provider.PriorityHigh)
	// FAC-9999 is referenced by the desired edge but never added to the store,
	// so TaskStatus on it errors with ErrDeletedTask -- no archive evidence exists.
	des := &Provenance{
		Version: SchemaVersion, TaskRef: "FAC-777", TaskID: "id-fac-777", Present: true,
		Edges: []DependencyEdge{{SourceRef: "FAC-9999", TargetRef: "FAC-777", Type: EdgeBlocks}},
	}
	_, err := ValidateLaunch(context.Background(), m, EntryDispatch, "FAC-777", des, "")
	if err == nil {
		t.Fatalf("unreadable prerequisite with no archive evidence must stay BLOCKED, got OK")
	}
}
