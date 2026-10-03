package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LifecycleRecord is the durable, portable ownership record for a managed
// worktree. Path is deliberately relative to the repository's state root: a
// record must remain valid when a repository or home directory moves.
type LifecycleRecord struct {
	Version    int       `json:"version"`
	Repository string    `json:"repository"`
	Owner      string    `json:"owner"`
	Kind       string    `json:"kind"`
	Task       string    `json:"task,omitempty"`
	Base       string    `json:"base,omitempty"`
	Path       string    `json:"path"`
	CreatedAt  time.Time `json:"created_at"`
	RetiredAt  time.Time `json:"retired_at,omitempty"`
	State      string    `json:"state"`
}

// StateWorktreeRoot is the one canonical location for a repository's managed
// checkouts. XDG_STATE_HOME may be supplied by a service manager or test; the
// fallback follows the XDG default used by Herdforge's other state.
func StateWorktreeRoot(repoRoot string) string {
	state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "herdforge", "worktrees", RepositoryID(repoRoot))
}

// RepositoryID is a stable, path-free state directory name. The readable
// basename makes operator inspection pleasant; the digest prevents collisions
// between repositories that share that basename.
func RepositoryID(repoRoot string) string {
	canonical, err := filepath.Abs(repoRoot)
	if err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
			canonical = resolved
		}
	}
	name := safeLifecyclePart(filepath.Base(canonical))
	if name == "" || name == "." {
		name = "repository"
	}
	digest := sha256.Sum256([]byte(filepath.Clean(canonical)))
	return name + "-" + hex.EncodeToString(digest[:])[:12]
}

func safeLifecyclePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

// NewStateWorktreeManager creates task worktrees outside the checkout at
// <state>/herdforge/worktrees/<repo-id>/task/<task>. Unlike the legacy
// constructor, it never computes a pool below RepoRoot.
func NewStateWorktreeManager(repoRoot string) *WorktreeManager {
	root := repoRoot
	if canonical, err := ResolveCanonicalRoot(context.Background(), repoRoot, ""); err == nil {
		root = canonical
	}
	stateRoot := StateWorktreeRoot(root)
	w := NewWorktreePool(root, filepath.Join(stateRoot, "task"))
	w.StateRoot = stateRoot
	w.Owner = "herd"
	w.Kind = "task"
	return w
}

func (w *WorktreeManager) lifecycleRecordPath(task string) (string, error) {
	if w == nil || strings.TrimSpace(w.StateRoot) == "" {
		return "", nil
	}
	kind := safeLifecyclePart(w.Kind)
	id := safeLifecyclePart(task)
	if kind == "" || id == "" {
		return "", fmt.Errorf("worktree lifecycle record requires kind and task")
	}
	return filepath.Join(w.StateRoot, "records", kind, id+".json"), nil
}

func (w *WorktreeManager) writeLifecycleRecord(task, base, path, state string, retiredAt time.Time) error {
	recordPath, err := w.lifecycleRecordPath(task)
	if err != nil || recordPath == "" {
		return err
	}
	rel, err := filepath.Rel(w.StateRoot, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("worktree lifecycle record path is outside state root")
	}
	record := LifecycleRecord{
		Version: 1, Repository: RepositoryID(w.RepoRoot), Owner: w.Owner,
		Kind: w.Kind, Task: task, Base: base, Path: filepath.ToSlash(rel),
		CreatedAt: time.Now().UTC(), RetiredAt: retiredAt.UTC(), State: state,
	}
	if existing, readErr := os.ReadFile(recordPath); readErr == nil {
		var prior LifecycleRecord
		if json.Unmarshal(existing, &prior) == nil && !prior.CreatedAt.IsZero() {
			record.CreatedAt = prior.CreatedAt
		}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lifecycle record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
		return fmt.Errorf("create lifecycle record directory: %w", err)
	}
	tmp := recordPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write lifecycle record: %w", err)
	}
	if err := os.Rename(tmp, recordPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish lifecycle record: %w", err)
	}
	return nil
}

func (w *WorktreeManager) recordActiveWorktree(task, base, path string) error {
	return w.writeLifecycleRecord(task, base, path, "active", time.Time{})
}

func (w *WorktreeManager) recordRetiredWorktree(path string) error {
	if w == nil || strings.TrimSpace(w.StateRoot) == "" {
		return nil
	}
	id := safeLifecyclePart(filepath.Base(path))
	return w.writeLifecycleRecord(id, "", path, "retired", time.Now().UTC())
}

// MigrateRecord records a legacy worktree moved into the managed state root.
// It is intentionally exported for the one-shot CLI migration, while normal
// creators use CreateTaskWorktree and cannot omit the record.
func (w *WorktreeManager) MigrateRecord(kind, id, owner, branch, path string) error {
	if w == nil || strings.TrimSpace(w.StateRoot) == "" {
		return fmt.Errorf("managed state root is required")
	}
	oldKind, oldOwner := w.Kind, w.Owner
	w.Kind, w.Owner = kind, owner
	defer func() { w.Kind, w.Owner = oldKind, oldOwner }()
	return w.recordActiveWorktree(id, branch, path)
}
