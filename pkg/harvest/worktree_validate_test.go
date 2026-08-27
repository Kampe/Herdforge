package harvest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyHarvestInputExcludesScratchpad(t *testing.T) {
	path := "/private/tmp/claude-501/-Users-kampe/scratchpad/mcp-nonvacuity-check"
	ok, reason := ClassifyHarvestInput("/repo", path)
	if ok || !strings.Contains(reason, "scratch") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
}

func TestClassifyHarvestInputExcludesMissingGit(t *testing.T) {
	dir := t.TempDir()
	ok, reason := ClassifyHarvestInput(dir, dir)
	if ok || !strings.Contains(reason, "not a git worktree") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
}

func TestHarvestSkipsScratchWithoutUNKNOWNError(t *testing.T) {
	// Build a fake listWorktrees by using a real repo with an added scratch-like path
	// that is NOT linked: we inject via Classify only through harvest filter unit.
	// Direct unit: skipped slice construction.
	root := t.TempDir()
	scratch := filepath.Join(root, "scratchpad", "mcp-nonvacuity-check")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	ok, reason := ClassifyHarvestInput(root, scratch)
	if ok {
		t.Fatal("scratch must be excluded")
	}
	if reason == "" {
		t.Fatal("exclusion must name a reason")
	}
}
