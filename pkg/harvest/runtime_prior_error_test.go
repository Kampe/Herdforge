package harvest

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectPriorPropagatesSpecificCause(t *testing.T) {
	f := retentionFixture(t)
	fakeSource := t.TempDir()
	i := f.installer
	i.Source = fakeSource
	_, err := i.inspectPrior(filepath.Join(f.root, "bin", "herd"))
	if err == nil {
		t.Fatal("inspectPrior on a non-git source must fail")
	}
	if !strings.Contains(err.Error(), "prior executable identity is unknown") {
		t.Fatalf("refusal context must be preserved, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not a Git worktree") {
		t.Fatalf("specific provenance cause must be propagated, got: %v", err)
	}
}
