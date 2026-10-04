package worktree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
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

func TestManagedLifecycleRetiresExactOwnerCheckout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	initRepo(t, repo)
	manager := NewStateWorktreeManagerFor(repo, "lane-cut", "lane-cut")
	path, err := manager.CreateManagedBranch(context.Background(), "FAC-992", "cut/fac-992", "HEAD")
	if err != nil {
		t.Fatalf("CreateManagedBranch: %v", err)
	}
	if !withinStateRoot(manager.StateRoot, path) {
		t.Fatalf("managed path escaped state root")
	}
	if err := manager.RetireManaged(context.Background(), "FAC-992"); err != nil {
		t.Fatalf("RetireManaged: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired checkout still exists: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(manager.StateRoot, "records", "lane-cut", "fac-992.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record LifecycleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "retired" || record.RetiredAt.IsZero() {
		t.Fatalf("retirement record = %+v", record)
	}
}

type lifecycleProcessInspector func(context.Context, string) (resources.ProcessUsage, error)

func (f lifecycleProcessInspector) InUse(ctx context.Context, path string) (resources.ProcessUsage, error) {
	return f(ctx, path)
}

func TestArchiveExpiredPreservesDirtyOwnerlessCheckout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	initRepo(t, repo)
	manager := NewStateWorktreeManagerFor(repo, "mutation-probe", "mutation-probe")
	path, err := manager.CreateManagedDetached(context.Background(), "probe-42", "HEAD")
	if err != nil {
		t.Fatalf("CreateManagedDetached: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "preserve.txt"), []byte("uncommitted evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(manager.StateRoot, "records", "mutation-probe", "probe-42.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record LifecycleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.CreatedAt = time.Now().Add(-48 * time.Hour).UTC()
	record.LastActivityAt = record.CreatedAt
	data, err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := ArchiveExpired(context.Background(), ExpiryPolicy{RepoRoot: repo, StateRoot: manager.StateRoot, InactiveFor: 24 * time.Hour, Now: time.Now, Processes: lifecycleProcessInspector(func(context.Context, string) (resources.ProcessUsage, error) { return resources.ProcessUsage{}, nil })})
	if err != nil {
		t.Fatalf("ArchiveExpired: %v", err)
	}
	if len(results) != 1 || !results[0].Archived || results[0].ArchiveRef != "refs/herd/archive/mutation-probe/probe-42" {
		t.Fatalf("archive results = %+v", results)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired checkout still exists: %v", err)
	}
	show := execCommandContext(context.Background(), "git", "-C", repo, "show", "refs/herd/archive/mutation-probe/probe-42:preserve.txt")
	output, err := show.CombinedOutput()
	if err != nil || string(output) != "uncommitted evidence\n" {
		t.Fatalf("archive ref did not preserve dirty file: err=%v output=%q", err, output)
	}
}

func TestArchiveExpiredRefusesLiveOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	initRepo(t, repo)
	manager := NewStateWorktreeManagerFor(repo, "review-surface", "review")
	path, err := manager.CreateManagedDetached(context.Background(), "fac-993", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(manager.StateRoot, "records", "review-surface", "fac-993.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record LifecycleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.LastActivityAt = time.Now().Add(-48 * time.Hour).UTC()
	data, err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := ArchiveExpired(context.Background(), ExpiryPolicy{RepoRoot: repo, StateRoot: manager.StateRoot, InactiveFor: time.Hour, Now: time.Now, Processes: lifecycleProcessInspector(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{CWD: true, PIDs: []int{1}}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Archived || results[0].Skipped != "live owner holds checkout" {
		t.Fatalf("live owner result = %+v", results)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live owner checkout was removed: %v", err)
	}
}
