package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// PoolSlot is the durable lease record for one warm worktree.
type PoolSlot struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Purpose  string    `json:"purpose,omitempty"`
	LeaseID  string    `json:"lease_id,omitempty"`
	LeasedAt time.Time `json:"leased_at,omitempty"`
	Base     string    `json:"base,omitempty"`
}

type poolState struct {
	Version int        `json:"version"`
	Slots   []PoolSlot `json:"slots"`
}

// Pool manages long-lived, dependency-bearing worktrees. The state file is
// deliberately repo-relative so it can be moved with a worktree.
type Pool struct {
	RepoRoot    string
	Root        string
	Size        int
	DefaultBase string
	Now         func() time.Time
}

func NewPool(repoRoot, root string, size int) *Pool {
	if size < 0 {
		size = 0
	}
	return &Pool{RepoRoot: repoRoot, Root: root, Size: size, DefaultBase: "origin/main", Now: time.Now}
}

func (p *Pool) statePath() string { return filepath.Join(p.Root, "pool.json") }
func (p *Pool) lockPath() string  { return filepath.Join(p.Root, "pool.lock") }

func (p *Pool) withLock(fn func() error) error {
	if p == nil || strings.TrimSpace(p.Root) == "" {
		return errors.New("worktree pool: root is required")
	}
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("worktree pool: create root: %w", err)
	}
	f, err := os.OpenFile(p.lockPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("worktree pool is busy or lock is stale: %w", err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(p.lockPath()) }()
	return fn()
}

func (p *Pool) readState() (poolState, error) {
	data, err := os.ReadFile(p.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return poolState{Version: 1, Slots: []PoolSlot{}}, nil
	}
	if err != nil {
		return poolState{}, fmt.Errorf("worktree pool: read state: %w", err)
	}
	var state poolState
	if err := json.Unmarshal(data, &state); err != nil {
		return poolState{}, fmt.Errorf("worktree pool: decode state: %w", err)
	}
	if state.Version != 1 {
		return poolState{}, fmt.Errorf("worktree pool: unsupported state version %d", state.Version)
	}
	return state, nil
}

func (p *Pool) writeState(state poolState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("worktree pool: encode state: %w", err)
	}
	tmp := p.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("worktree pool: write state: %w", err)
	}
	if err := os.Rename(tmp, p.statePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("worktree pool: publish state: %w", err)
	}
	return nil
}

// Ensure creates the configured slot directories and durable inventory. It
// does not claim slots; callers must still use Lease before writing to one.
func (p *Pool) Ensure(ctx context.Context) error {
	return p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(state.Slots))
		for _, slot := range state.Slots {
			seen[slot.Name] = true
		}
		for i := 0; i < p.Size; i++ {
			name := fmt.Sprintf("pool-%02d", i+1)
			if seen[name] {
				continue
			}
			state.Slots = append(state.Slots, PoolSlot{Name: name, Path: filepath.Join(p.Root, name)})
		}
		sort.Slice(state.Slots, func(i, j int) bool { return state.Slots[i].Name < state.Slots[j].Name })
		base := p.DefaultBase
		if base == "" {
			base = "origin/main"
		}
		for _, slot := range state.Slots {
			_, statErr := os.Stat(slot.Path)
			if statErr == nil {
				continue
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("worktree pool: inspect %s: %w", slot.Name, statErr)
			}
			if err := os.MkdirAll(filepath.Dir(slot.Path), 0o755); err != nil {
				return fmt.Errorf("worktree pool: create parent: %w", err)
			}
			cmd := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "add", "--detach", slot.Path, base)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: create %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
		}
		return p.writeState(state)
	})
}

// Lease claims the first available clean slot. Dirty or uninspectable slots
// are never handed out, because a review must execute against known contents.
func (p *Pool) Lease(ctx context.Context, purpose string) (*PoolSlot, error) {
	if strings.TrimSpace(purpose) == "" {
		return nil, errors.New("worktree pool: purpose is required")
	}
	var result *PoolSlot
	err := p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for i := range state.Slots {
			slot := &state.Slots[i]
			if slot.LeaseID != "" {
				continue
			}
			clean, err := gitClean(ctx, p.RepoRoot, slot.Path)
			if err != nil {
				return fmt.Errorf("worktree pool: inspect %s: %w", slot.Name, err)
			}
			if !clean {
				return fmt.Errorf("worktree pool: slot %s is dirty; refusing lease", slot.Name)
			}
			stamp := p.Now
			if stamp == nil {
				stamp = time.Now
			}
			slot.Purpose, slot.LeasedAt = purpose, stamp().UTC()
			slot.LeaseID = fmt.Sprintf("%s-%d", slot.Name, slot.LeasedAt.UnixNano())
			copy := *slot
			result = &copy
			return p.writeState(state)
		}
		return errors.New("worktree pool: no available clean slots")
	})
	return result, err
}

// Release resets a leased slot to the configured base and verifies it is
// clean before returning it to the pool. A failed reset leaves the lease held.
func (p *Pool) Release(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("worktree pool: lease id is required")
	}
	return p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for i := range state.Slots {
			slot := &state.Slots[i]
			if slot.LeaseID != leaseID {
				continue
			}
			base := p.DefaultBase
			if base == "" {
				base = "origin/main"
			}
			cmd := exec.CommandContext(ctx, "git", "-C", slot.Path, "reset", "--hard", base)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: reset %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			cmd = exec.CommandContext(ctx, "git", "-C", slot.Path, "clean", "-fd")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: clean %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			clean, err := gitClean(ctx, p.RepoRoot, slot.Path)
			if err != nil {
				return err
			}
			if !clean {
				return fmt.Errorf("worktree pool: slot %s remains dirty after release", slot.Name)
			}
			slot.Purpose, slot.LeaseID = "", ""
			slot.LeasedAt = time.Time{}
			return p.writeState(state)
		}
		return errors.New("worktree pool: lease not found")
	})
}

// GC tears down every unleased slot so the next Ensure rebuilds the pool.
// Leased slots are preserved and make the operation fail closed.
func (p *Pool) GC(ctx context.Context) error {
	return p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for _, slot := range state.Slots {
			if slot.LeaseID != "" {
				return fmt.Errorf("worktree pool: gc refused while slot %s is leased", slot.Name)
			}
			if err := RefuseRemovalWithLiveLease(ctx, p.RepoRoot, slot.Path); err != nil {
				return fmt.Errorf("worktree pool: gc lease fence for %s: %w", slot.Name, err)
			}
			cmd := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "remove", "--force", slot.Path)
			if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "is not a working tree") {
				return fmt.Errorf("worktree pool: remove %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			if err := os.RemoveAll(slot.Path); err != nil {
				return fmt.Errorf("worktree pool: remove slot %s: %w", slot.Name, err)
			}
		}
		return p.writeState(state)
	})
}

// Slots returns a snapshot of durable pool inventory.
func (p *Pool) Slots() ([]PoolSlot, error) {
	state, err := p.readState()
	if err != nil {
		return nil, err
	}
	return append([]PoolSlot(nil), state.Slots...), nil
}

func gitClean(ctx context.Context, repoRoot, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git status %s: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// SeedClone copies a warm template using APFS clonefiles when available, with
// a portable recursive-copy fallback. The destination must not already exist.
func SeedClone(ctx context.Context, source, destination string) error {
	if source == "" || destination == "" {
		return errors.New("worktree pool: source and destination are required")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("worktree pool: destination already exists: %s", destination)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		if err := exec.CommandContext(ctx, "cp", "-cR", source, destination).Run(); err == nil {
			return nil
		}
	}
	return copyTree(source, destination)
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
