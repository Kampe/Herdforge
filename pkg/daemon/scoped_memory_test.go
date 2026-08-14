package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/memory"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestDaemonPacketWithMemory_InjectsOnlyCurrentPromotedKnowledge(t *testing.T) {
	store, err := memory.NewScopedMemoryStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	worker := memory.Actor{ID: "worker", Role: "worker", Authenticated: true}
	current := memory.Scope{Kind: memory.ScopeTask, RunID: "run-1", TaskID: "FAC-239", Role: "builder", Revision: "graph-r1", Readers: []string{"worker"}, Writers: []string{"worker"}}
	other := memory.Scope{Kind: memory.ScopeTask, RunID: "run-1", TaskID: "FAC-240", Role: "builder", Revision: "graph-r1", Readers: []string{"worker"}, Writers: []string{"worker"}}
	stale := memory.Scope{Kind: memory.ScopeTask, RunID: "run-1", TaskID: "FAC-241", Role: "builder", Revision: "graph-r0", Readers: []string{"worker"}, Writers: []string{"worker"}}
	for _, scope := range []memory.Scope{current, other, stale} {
		if err := store.RegisterScope(scope); err != nil {
			t.Fatal(err)
		}
	}
	propose := func(scope memory.Scope, content string) memory.Proposal {
		p, err := store.Propose(memory.ProposalRequest{Scope: scope, Actor: worker, Content: content, SourceEvidence: "receipt:" + content, CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetainUntil: now.Add(2 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	allowed := propose(current, "reviewed global lesson")
	_ = propose(other, "other task proposal")
	_ = propose(stale, "stale proposal")
	if _, err := store.Promote(memory.PromotionRequest{ProposalID: allowed.ID, Actor: memory.Actor{ID: "reviewer", Role: "reviewer", Authenticated: true}, Revision: "graph-r1", TaskEvidence: "task:FAC-239", PromotedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	packet, err := daemonPacketWithMemory(&provider.Task{Ref: "FAC-239", Title: "task"}, &config.LaneDef{Name: "builder"}, "./worktree", "graph-r1", store, worker, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(packet, "reviewed global lesson") {
		t.Fatalf("promoted current knowledge missing from packet: %q", packet)
	}
	if strings.Contains(packet, "other task proposal") || strings.Contains(packet, "stale proposal") {
		t.Fatalf("cross-task or stale memory leaked into packet: %q", packet)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	readCount := 0
	for _, record := range evidence {
		if record.Action == "read" && record.TaskID == "FAC-239" && record.Revision == "graph-r1" {
			readCount++
		}
	}
	if readCount != 2 {
		t.Fatalf("daemon packet path did not persist a read receipt for each injected record: %+v", evidence)
	}
}

func TestDaemonPacketWithMemory_SameTaskStaleRevisionIsExplicitlyExcluded(t *testing.T) {
	store, err := memory.NewScopedMemoryStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := memory.Actor{ID: "worker", Role: "worker", Authenticated: true}
	stale := memory.Scope{Kind: memory.ScopeTask, RunID: "run-1", TaskID: "FAC-239", Role: "builder", Revision: "graph-r0", Readers: []string{"worker"}, Writers: []string{"worker"}}
	if err := store.RegisterScope(stale); err != nil {
		t.Fatal(err)
	}
	packet, err := daemonPacketWithMemory(&provider.Task{Ref: "FAC-239", Title: "task"}, &config.LaneDef{Name: "builder"}, "./worktree", "graph-r1", store, worker, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(packet, "Scoped memory excluded: stale revision.") {
		t.Fatalf("stale exclusion must be explicit in the task packet: %q", packet)
	}
}
