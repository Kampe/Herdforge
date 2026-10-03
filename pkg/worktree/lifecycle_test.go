package worktree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateWorktreeManagerCreatesOutsideRepositoryAndRecordsOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	initRepo(t, repo)

	manager := NewStateWorktreeManager(repo)
	info, err := manager.CreateTaskWorktree(context.Background(), "FAC-991")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	if !withinStateRoot(manager.StateRoot, info.Path) {
		t.Fatalf("worktree %q is not below state root %q", info.Path, manager.StateRoot)
	}
	if withinStateRoot(repo, info.Path) {
		t.Fatalf("worktree %q must not be below repository %q", info.Path, repo)
	}

	recordPath := filepath.Join(manager.StateRoot, "records", "task", "fac-991.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read lifecycle record: %v", err)
	}
	var record LifecycleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode lifecycle record: %v", err)
	}
	if record.State != "active" || record.Owner != "herd" || record.Kind != "task" || record.Task != "FAC-991" {
		t.Fatalf("unexpected lifecycle record: %+v", record)
	}
	if record.Path != "task/fac-991" || filepath.IsAbs(record.Path) || strings.Contains(string(data), repo) {
		t.Fatalf("record must retain only a portable state-relative path: %s", data)
	}
}

func withinStateRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
