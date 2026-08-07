package feedback

import (
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestResolveWorkspaceEnvOverrideWinsWithoutCallingList(t *testing.T) {
	called := false
	list := func() ([]herdr.WorkspaceEntry, error) { called = true; return nil, nil }
	got, err := ResolveWorkspace("/repo", "", "wOverride", list)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wOverride" {
		t.Fatalf("got %q, want wOverride", got)
	}
	if called {
		t.Fatal("an explicit override must not consult the workspace list at all")
	}
}

func TestResolveWorkspaceExactLabelMatchCaseInsensitive(t *testing.T) {
	list := func() ([]herdr.WorkspaceEntry, error) {
		return []herdr.WorkspaceEntry{
			{WorkspaceID: "w1", Label: "Foo"},
			{WorkspaceID: "w2", Label: "foo-bar"}, // must never substring-match "foo"
		}, nil
	}
	got, err := ResolveWorkspace("/repo", "foo", "", list)
	if err != nil {
		t.Fatal(err)
	}
	if got != "w1" {
		t.Fatalf("got %q, want w1 (case-insensitive exact match, never substring)", got)
	}
}

func TestResolveWorkspaceCwdUnderRoot(t *testing.T) {
	list := func() ([]herdr.WorkspaceEntry, error) {
		return []herdr.WorkspaceEntry{
			{WorkspaceID: "w1", Label: "unrelated", Cwd: "/other"},
			{WorkspaceID: "w2", Label: "unrelated2", Cwd: "/repo/worktree"},
		}, nil
	}
	got, err := ResolveWorkspace("/repo", "no-label-match", "", list)
	if err != nil {
		t.Fatal(err)
	}
	if got != "w2" {
		t.Fatalf("got %q, want w2 (cwd nested under root)", got)
	}
}

func TestResolveWorkspaceSingleWorkspaceFallback(t *testing.T) {
	list := func() ([]herdr.WorkspaceEntry, error) {
		return []herdr.WorkspaceEntry{{WorkspaceID: "only", Label: "nothing-matches"}}, nil
	}
	got, err := ResolveWorkspace("/repo", "no-match", "", list)
	if err != nil {
		t.Fatal(err)
	}
	if got != "only" {
		t.Fatalf("got %q, want only (single-workspace last resort)", got)
	}
}

func TestResolveWorkspaceUnresolvedFailsClosed(t *testing.T) {
	list := func() ([]herdr.WorkspaceEntry, error) {
		return []herdr.WorkspaceEntry{
			{WorkspaceID: "w1", Label: "a"},
			{WorkspaceID: "w2", Label: "b"},
		}, nil
	}
	_, err := ResolveWorkspace("/repo", "no-match", "", list)
	if !errors.Is(err, ErrWorkspaceUnknown) {
		t.Fatalf("err = %v, want ErrWorkspaceUnknown", err)
	}
}

func TestResolveWorkspaceListErrorPropagates(t *testing.T) {
	sentinel := errors.New("herdr unavailable")
	list := func() ([]herdr.WorkspaceEntry, error) { return nil, sentinel }
	_, err := ResolveWorkspace("/repo", "", "", list)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestCoordinatorTargetMatchesFirstQualifyingAgent(t *testing.T) {
	agents := []herdr.AgentEntry{
		{Name: "worker-1", Workspace: "wF"},
		{Name: "chainseer-orchestrator", Workspace: "wF", PaneID: "pane-9"},
	}
	if got := CoordinatorTarget(agents, "wF"); got != "chainseer-orchestrator" {
		t.Fatalf("got %q, want chainseer-orchestrator", got)
	}
}

func TestCoordinatorTargetCaseInsensitive(t *testing.T) {
	agents := []herdr.AgentEntry{{Name: "Coordinator", Workspace: "wF", PaneID: "pane-9"}}
	if got := CoordinatorTarget(agents, "wF"); got != "Coordinator" {
		t.Fatalf("got %q, want Coordinator (case-insensitive match)", got)
	}
}

func TestCoordinatorTargetUnnamedAgentNeverMatches(t *testing.T) {
	agents := []herdr.AgentEntry{{Name: "", Workspace: "wF", PaneID: "pane-9"}}
	if got := CoordinatorTarget(agents, "wF"); got != "" {
		t.Fatalf("got %q, want empty: an unnamed agent can never match coordinator|orchestrator", got)
	}
}

func TestCoordinatorTargetScopedToWorkspace(t *testing.T) {
	agents := []herdr.AgentEntry{{Name: "orchestrator", Workspace: "wOther"}}
	if got := CoordinatorTarget(agents, "wF"); got != "" {
		t.Fatalf("got %q, want empty (no match in workspace wF)", got)
	}
}

func TestCoordinatorTargetNoMatchIsEmpty(t *testing.T) {
	agents := []herdr.AgentEntry{{Name: "worker-1", Workspace: "wF"}}
	if got := CoordinatorTarget(agents, "wF"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
