package herdr

import (
	"strings"
	"testing"
)

// TestCloseOwnTabDelegatesWhenNoAgentRow is the FAC-550 regression. A dispatch
// that fails before its agent registers has no fencing evidence; the old
// TabClose stub refused unconditionally and leaked the tab.
func TestCloseOwnTabDelegatesWhenNoAgentRow(t *testing.T) {
	closed := ""
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "list":
			return `{"result":{"type":"agents","agents":[]}}`, nil
		case len(args) > 1 && args[0] == "tab" && args[1] == "close":
			closed = args[len(args)-1]
			return `{"result":{"type":"ok"}}`, nil
		case len(args) > 1 && args[0] == "tab" && args[1] == "list":
			// Absence readback: the tab is gone.
			return `{"result":{"type":"tabs","tabs":[{"tab_id":"wK:tOTHER","workspace_id":"wK"}]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	if err := CloseOwnTab("wK:tMF"); err != nil {
		t.Fatalf("compensating close must not leak the tab: %v", err)
	}
	if !strings.Contains(closed, "wK:tMF") {
		t.Fatalf("must close the exact tab it created, closed %q", closed)
	}
}

// TestCloseOwnTabFailsWhenTabSurvives keeps the close honest: a delegated close
// that leaves the tab present must not report success.
func TestCloseOwnTabFailsWhenTabSurvives(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "list":
			return `{"result":{"type":"agents","agents":[]}}`, nil
		case len(args) > 1 && args[0] == "tab" && args[1] == "list":
			return `{"result":{"type":"tabs","tabs":[{"tab_id":"wK:tMF","workspace_id":"wK"}]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	err := CloseOwnTab("wK:tMF")
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("a surviving tab must fail closed, got %v", err)
	}
}

func TestCloseOwnTabRequiresTabID(t *testing.T) {
	if err := CloseOwnTab("  "); err == nil {
		t.Fatal("an empty tab id must fail closed")
	}
}
