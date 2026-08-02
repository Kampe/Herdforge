package herdr

import "testing"

func TestPickWorkspace(t *testing.T) {
	entries := []WorkspaceEntry{
		{WorkspaceID: "w1", Label: "kluster"},
		{WorkspaceID: "wF", Label: "Herdforge"},
		{WorkspaceID: "w3", Label: "dotfiles", Focused: true},
	}
	if got := PickWorkspace(entries, "herdforge"); got != "wF" {
		t.Errorf("label match (case-insensitive) = %q, want wF", got)
	}
	if got := PickWorkspace(entries, "unknown-repo"); got != "w3" {
		t.Errorf("focused fallback = %q, want w3", got)
	}
	if got := PickWorkspace([]WorkspaceEntry{{WorkspaceID: "w9", Label: "x"}}, "y"); got != "w9" {
		t.Errorf("first fallback = %q, want w9", got)
	}
	if got := PickWorkspace(nil, "y"); got != "wF" {
		t.Errorf("empty fallback = %q, want wF", got)
	}
}
