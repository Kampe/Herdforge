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

	"github.com/Kampe/Herdforge/pkg/resources"
)

// PoolSlot is the durable lease record for one warm worktree.
type PoolSlot struct {
	Name                  string    `json:"name"`
	Path                  string    `json:"path"`
	Purpose               string    `json:"purpose,omitempty"`
	LeaseID               string    `json:"lease_id,omitempty"`
	LeasedAt              time.Time `json:"leased_at,omitempty"`
	Base                  string    `json:"base,omitempty"`
	LastReleaseLeaseID    string    `json:"last_release_lease_id,omitempty"`
	LastReleaseGeneration int64     `json:"last_release_generation,omitempty"`
	LastReleasePath       string    `json:"last_release_path,omitempty"`
	LastReleaseTargetHead string    `json:"last_release_target_head,omitempty"`
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
	// HolderLive reports whether a lease's holder is still a live, unsettled
	// worker. It is injected so this package stays free of any dependency on the
	// terminal plane.
	//
	// FAC-591: without it the pool had no way to tell a working reviewer from a
	// dead one. Every lease taken by a launch that later died stayed held
	// forever — GC refuses while anything is leased, and Release needs a lease
	// id nobody recorded — so the pool wedged at "no available clean slots" with
	// zero live reviewers and reported itself saturated. That happened
	// repeatedly and each time needed a human to unstick it.
	HolderLive func(purpose string) bool
	// ProcessInspector is the native process/cwd/open-handle census used before
	// removing an unleased slot. Unsupported or incomplete inspection is a
	// refusal, never evidence that the slot is idle. Tests may inject a
	// deterministic fixture; production defaults to the bounded native census.
	ProcessInspector resources.ProcessInspector
}

func NewPool(repoRoot, root string, size int) *Pool {
	if size < 0 {
		size = 0
	}
	return &Pool{RepoRoot: repoRoot, Root: root, Size: size, DefaultBase: "origin/main", Now: time.Now,
		ProcessInspector: resources.LSOFProcessInspector{Timeout: 2 * time.Second}}
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
		// FAC-591: before declaring the pool full, reclaim any lease whose holder
		// is gone. A launch that dies after leasing leaves an ownerless lease
		// that nothing else can free, so without this the pool wedges
		// permanently and reports saturation with no reviewer running.
		reclaimed, rerr := p.reclaimDeadLocked(ctx, state)
		if rerr != nil {
			return rerr
		}
		if len(reclaimed) == 0 {
			return errors.New("worktree pool: no available clean slots")
		}
		if err := p.writeState(state); err != nil {
			return err
		}
		for i := range state.Slots {
			slot := &state.Slots[i]
			if slot.LeaseID != "" {
				continue
			}
			clean, err := gitClean(ctx, p.RepoRoot, slot.Path)
			if err != nil || !clean {
				continue
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
		return fmt.Errorf("worktree pool: reclaimed %d dead lease(s) but no slot became leasable", len(reclaimed))
	})
	return result, err
}

// reclaimDeadLocked frees leases whose holder is no longer live. The caller
// must hold the pool lock and must persist state afterwards. It returns the
// names of the slots it freed.
//
// A slot whose reset fails keeps its lease: handing out a dirty tree is worse
// than reporting the pool full, because the reviewer would read the wrong
// content and its verdict would be about a commit nobody asked for.
func (p *Pool) reclaimDeadLocked(ctx context.Context, state poolState) ([]string, error) {
	if p.HolderLive == nil {
		return nil, nil
	}
	base := p.DefaultBase
	if base == "" {
		base = "origin/main"
	}
	var freed []string
	for i := range state.Slots {
		slot := &state.Slots[i]
		if slot.LeaseID == "" {
			continue
		}
		// An empty purpose cannot be attributed to any holder, so it can never
		// be proven live and would otherwise be held forever.
		if strings.TrimSpace(slot.Purpose) != "" && p.HolderLive(slot.Purpose) {
			continue
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", slot.Path, "reset", "--hard", base).CombinedOutput(); err != nil {
			return freed, fmt.Errorf("worktree pool: reclaim reset %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", slot.Path, "clean", "-fd").CombinedOutput(); err != nil {
			return freed, fmt.Errorf("worktree pool: reclaim clean %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
		}
		clean, err := gitClean(ctx, p.RepoRoot, slot.Path)
		if err != nil {
			return freed, err
		}
		if !clean {
			continue
		}
		headOut, err := exec.CommandContext(ctx, "git", "-C", slot.Path, "rev-parse", "HEAD").Output()
		if err != nil {
			return freed, fmt.Errorf("worktree pool: reclaim rev-parse %s: %w", slot.Name, err)
		}
		slot.LastReleaseLeaseID, slot.LastReleaseGeneration, slot.LastReleasePath = slot.LeaseID, slot.LeasedAt.UnixNano(), slot.Path
		slot.LastReleaseTargetHead = strings.TrimSpace(string(headOut))
		slot.Purpose, slot.LeaseID = "", ""
		slot.LeasedAt = time.Time{}
		freed = append(freed, slot.Name)
	}
	return freed, nil
}

// ReclaimDead frees every lease whose holder is no longer live and reports the
// slots it freed. Exposed so an operator or a sweep can unstick the pool
// without taking a lease.
func (p *Pool) ReclaimDead(ctx context.Context) ([]string, error) {
	var freed []string
	err := p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		freed, err = p.reclaimDeadLocked(ctx, state)
		if err != nil {
			return err
		}
		if len(freed) == 0 {
			return nil
		}
		return p.writeState(state)
	})
	return freed, err
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
			headOut, err := exec.CommandContext(ctx, "git", "-C", slot.Path, "rev-parse", "HEAD").Output()
			if err != nil {
				return fmt.Errorf("worktree pool: rev-parse %s: %w", slot.Name, err)
			}
			slot.LastReleaseLeaseID, slot.LastReleaseGeneration, slot.LastReleasePath = slot.LeaseID, slot.LeasedAt.UnixNano(), slot.Path
			slot.LastReleaseTargetHead = strings.TrimSpace(string(headOut))
			slot.Purpose, slot.LeaseID = "", ""
			slot.LeasedAt = time.Time{}
			return p.writeState(state)
		}
		return errors.New("worktree pool: lease not found")
	})
}

// ReleaseExact releases only the named slot when its path, nonce, and lease
// incarnation all match the caller's authenticated manifest. A nonce alone
// is insufficient because a recycled pool can legally reuse identifiers.
func (p *Pool) ReleaseExact(ctx context.Context, slotName, leaseID string, leaseGeneration int64, wantPath string) error {
	if strings.TrimSpace(slotName) == "" || strings.TrimSpace(leaseID) == "" || leaseGeneration <= 0 || strings.TrimSpace(wantPath) == "" {
		return errors.New("worktree pool: exact release requires slot, lease, generation, and path")
	}
	return p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for i := range state.Slots {
			slot := &state.Slots[i]
			if slot.Name != slotName {
				continue
			}
			slotPath := p.repoPath(slot.Path)
			if slotPath != p.repoPath(wantPath) {
				return fmt.Errorf("worktree pool: slot %s path changed", slotName)
			}
			if slot.LeaseID == "" {
				return nil
			}
			if slot.LeaseID != leaseID || slot.LeasedAt.UnixNano() != leaseGeneration {
				return fmt.Errorf("worktree pool: slot %s lease incarnation changed", slotName)
			}
			base := slot.Base
			if base == "" {
				base = p.DefaultBase
				if base == "" {
					base = "origin/main"
				}
			}
			cmd := exec.CommandContext(ctx, "git", "-C", slotPath, "reset", "--hard", base)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: reset %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			cmd = exec.CommandContext(ctx, "git", "-C", slotPath, "clean", "-fd")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: clean %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			clean, err := gitClean(ctx, p.RepoRoot, slotPath)
			if err != nil {
				return err
			}
			if !clean {
				return fmt.Errorf("worktree pool: slot %s remains dirty after release", slot.Name)
			}
			headOut, err := exec.CommandContext(ctx, "git", "-C", slotPath, "rev-parse", "HEAD").Output()
			if err != nil {
				return fmt.Errorf("worktree pool: rev-parse %s: %w", slot.Name, err)
			}
			slot.LastReleaseLeaseID, slot.LastReleaseGeneration, slot.LastReleasePath = slot.LeaseID, slot.LeasedAt.UnixNano(), slot.Path
			slot.LastReleaseTargetHead = strings.TrimSpace(string(headOut))
			slot.Purpose, slot.LeaseID, slot.LeasedAt = "", "", time.Time{}
			return p.writeState(state)
		}
		return errors.New("worktree pool: exact slot not found")
	})
}

// RetireExact removes one already-released owned slot and preserves every
// other slot in the pool. It is intentionally narrower than GC.
func (p *Pool) RetireExact(ctx context.Context, slotName, wantPath, expectedLeaseID string, expectedGeneration int64) error {
	if strings.TrimSpace(slotName) == "" || strings.TrimSpace(wantPath) == "" || strings.TrimSpace(expectedLeaseID) == "" || expectedGeneration <= 0 {
		return errors.New("worktree pool: exact retirement requires slot and path")
	}
	err := p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for i := range state.Slots {
			slot := state.Slots[i]
			if slot.Name != slotName {
				continue
			}
			slotPath := p.repoPath(slot.Path)
			if slotPath != p.repoPath(wantPath) {
				return fmt.Errorf("worktree pool: slot %s path changed", slotName)
			}
			if slot.LeaseID != "" {
				return fmt.Errorf("worktree pool: slot %s is still leased", slotName)
			}
			if slot.LastReleaseLeaseID != expectedLeaseID || slot.LastReleaseGeneration != expectedGeneration || p.repoPath(slot.LastReleasePath) != slotPath {
				return fmt.Errorf("worktree pool: slot %s release incarnation changed", slotName)
			}
			cmd := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "remove", "--force", slotPath)
			if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "is not a working tree") {
				return fmt.Errorf("worktree pool: remove %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			if err := os.RemoveAll(slotPath); err != nil {
				return err
			}
			state.Slots = append(state.Slots[:i], state.Slots[i+1:]...)
			if err := p.writeState(state); err != nil {
				return err
			}
			if len(state.Slots) == 0 {
				_ = os.Remove(p.statePath())
			}
			return nil
		}
		return nil
	})
	if err == nil {
		if entries, readErr := os.ReadDir(p.Root); readErr == nil && len(entries) == 0 {
			_ = os.Remove(p.Root)
		}
	}
	return err
}

func (p *Pool) repoPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(p.RepoRoot, path))
}

// GC tears down every unleased, verified-safe slot so the next Ensure
// rebuilds the pool. Leased slots are preserved and make the operation fail
// closed.
//
// FAC-717: the previous version force-removed and os.RemoveAll'd every
// unleased slot.Path exactly as recorded, without normalizing it through
// repoPath, without confirming it was still the exact worktree git itself
// has registered, without checking for a symlink swap, and without checking
// for untracked/ignored user work or that the slot's HEAD was still fully
// contained in the pool's history. A relative slot.Path loaded from a
// corrupt or hand-edited state file resolved against the process's own
// working directory rather than the repository root, and --force bypassed
// git's own dirty-tree refusal entirely. GC now verifies the complete
// selected set (path containment, exact registered worktree identity, no
// symlink, full cleanliness including ignored files, and commit
// reachability from base) before removing anything, and immediately
// re-verifies each slot right before its own destructive step, since
// filesystem removal cannot be rolled back and state can change between the
// two passes.
func (p *Pool) GC(ctx context.Context) error {
	return p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		verifiedCandidates, err := p.verifyGCSet(ctx, state)
		if err != nil {
			return err
		}
		removed := make(map[int]bool, len(verifiedCandidates))
		for i, slot := range state.Slots {
			candidate, ok := verifiedCandidates[i]
			if !ok {
				continue
			}
			// Re-verify this one slot immediately before removing it: the
			// full-set pass above proves nothing about state a moment later.
			latest, err := p.verifyGCCandidate(ctx, slot)
			if err != nil {
				return fmt.Errorf("worktree pool: gc refused for slot %s immediately before removal: %w", slot.Name, err)
			}
			if err := candidate.sameIdentity(latest); err != nil {
				return fmt.Errorf("worktree pool: gc refused for slot %s immediately before removal: %w", slot.Name, err)
			}
			// Pool slots are tracked by their own slot.LeaseID, verified
			// above, never by pkg/claim -- the full RefuseRemovalWithLiveLease
			// would refuse every slot unconditionally. See its doc comment.
			// No --force: verifyGCCandidate already proved this exact,
			// symlink-free, contained path is a clean registered worktree, so
			// git's own dirty/HEAD-mismatch refusal is redundant defense-in-
			// depth, not an obstacle to bypass.
			cmd := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "remove", candidate.path)
			if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "is not a working tree") {
				return fmt.Errorf("worktree pool: remove %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			if info, statErr := os.Lstat(candidate.path); statErr == nil {
				if !os.SameFile(candidate.info, info) {
					return fmt.Errorf("worktree pool: slot %s path was replaced after git removal; preserving it", slot.Name)
				}
				return fmt.Errorf("worktree pool: slot %s remained after git removal; preserving it", slot.Name)
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("worktree pool: inspect slot %s after git removal: %w", slot.Name, statErr)
			}
			removed[i] = true
		}
		kept := state.Slots[:0]
		for i, slot := range state.Slots {
			if !removed[i] {
				kept = append(kept, slot)
			}
		}
		state.Slots = kept
		return p.writeState(state)
	})
}

// GCPlan reports, without deleting anything, which currently-tracked slots
// GC would remove and which it would refuse, and why. It runs the identical
// verification GC uses so a dry run cannot diverge from the real decision.
func (p *Pool) GCPlan(ctx context.Context) ([]GCDecision, error) {
	var decisions []GCDecision
	err := p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for _, slot := range state.Slots {
			d := GCDecision{Slot: slot.Name, Path: slot.Path}
			if slot.LeaseID != "" {
				d.Refused = true
				d.Reason = "leased"
			} else if _, err := p.verifyGCCandidate(ctx, slot); err != nil {
				d.Refused = true
				d.Reason = err.Error()
			}
			decisions = append(decisions, d)
		}
		return nil
	})
	return decisions, err
}

// GCDecision is one slot's read-only GC disposition, for --dry-run reporting.
type GCDecision struct {
	Slot    string
	Path    string
	Refused bool
	Reason  string
}

// verifyGCSet validates every unleased slot in state before GC removes the
// first one, so a later leased/unsafe slot is never discovered only after
// earlier pool worktrees were already deleted. It returns the verified,
// normalized removal path for each slot index that passed.
type gcCandidate struct {
	path string
	info os.FileInfo
	head string
}

func (c gcCandidate) sameIdentity(other gcCandidate) error {
	if c.path != other.path {
		return fmt.Errorf("slot path changed from %s to %s", c.path, other.path)
	}
	if c.head != other.head {
		return fmt.Errorf("registered Git HEAD changed from %s to %s", c.head, other.head)
	}
	if !os.SameFile(c.info, other.info) {
		return errors.New("slot directory identity changed")
	}
	return nil
}

func (p *Pool) verifyGCSet(ctx context.Context, state poolState) (map[int]gcCandidate, error) {
	verified := make(map[int]gcCandidate, len(state.Slots))
	for i, slot := range state.Slots {
		if slot.LeaseID != "" {
			return nil, fmt.Errorf("worktree pool: gc refused while slot %s is leased", slot.Name)
		}
		candidate, err := p.verifyGCCandidate(ctx, slot)
		if err != nil {
			return nil, fmt.Errorf("worktree pool: gc refused for slot %s: %w", slot.Name, err)
		}
		verified[i] = candidate
	}
	return verified, nil
}

// verifyGCCandidate proves a single pool slot is safe to destroy. It refuses
// (never guesses safe) on: an empty or unresolvable path; a path that
// normalizes outside the pool root; a symlinked leaf; a path with no
// lease-history fence clearance; a path git does not report as an exact
// registered worktree of this repository; a working tree that is not fully
// clean, including untracked and git-ignored content; and a HEAD that is
// not fully reachable from the pool's base ref, since destroying it could
// discard commits that exist nowhere else. It returns the normalized,
// contained path to remove.
func (p *Pool) verifyGCCandidate(ctx context.Context, slot PoolSlot) (gcCandidate, error) {
	if strings.TrimSpace(slot.Path) == "" {
		return gcCandidate{}, errors.New("slot has no recorded path")
	}
	resolved, err := p.containedRepoPath(slot.Path)
	if err != nil {
		return gcCandidate{}, err
	}
	if err := RefuseRemovalWithoutLeaseHistoryCheck(ctx, p.RepoRoot, resolved); err != nil {
		return gcCandidate{}, err
	}
	registered, err := registeredWorktrees(ctx, p.RepoRoot)
	if err != nil {
		return gcCandidate{}, fmt.Errorf("list registered worktrees: %w", err)
	}
	head, ok := registered[resolved]
	if !ok {
		return gcCandidate{}, fmt.Errorf("path is not a registered git worktree of this repository: %s", resolved)
	}
	clean, err := gitFullyClean(ctx, resolved)
	if err != nil {
		return gcCandidate{}, fmt.Errorf("inspect cleanliness: %w", err)
	}
	if !clean {
		return gcCandidate{}, fmt.Errorf("slot is dirty (including untracked or ignored content), refusing")
	}
	if err := p.inspectSlotUse(ctx, resolved); err != nil {
		return gcCandidate{}, err
	}
	base := p.DefaultBase
	if base == "" {
		base = "origin/main"
	}
	if err := verifyReachableFromBase(ctx, p.RepoRoot, resolved, base); err != nil {
		return gcCandidate{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return gcCandidate{}, fmt.Errorf("inspect slot identity: %w", err)
	}
	return gcCandidate{path: resolved, info: info, head: head}, nil
}

func (p *Pool) inspectSlotUse(ctx context.Context, path string) error {
	if p.ProcessInspector == nil {
		return errors.New("worktree pool: process census unavailable; refusing GC")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	usage, err := p.ProcessInspector.InUse(probeCtx, path)
	if err != nil {
		return fmt.Errorf("worktree pool: process census unavailable for %s: %w", path, err)
	}
	if usage.MetadataUnavailable {
		return fmt.Errorf("worktree pool: process census metadata unavailable for %s", path)
	}
	if usage.CWD || usage.OpenFile || usage.ReferencedPath {
		return fmt.Errorf("worktree pool: live process owns or references %s, refusing", path)
	}
	return nil
}

// containedRepoPath normalizes a possibly-relative slot path against the
// repository root (never the process's own working directory), then proves
// the resolved path is neither a symlink itself nor reachable only through
// one that escapes the pool root. It returns the normalized, non-symlink-
// resolved absolute path, which is what git's own worktree registry keys on.
func (p *Pool) containedRepoPath(path string) (string, error) {
	resolved := p.repoPath(path)
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("slot path does not exist: %s", resolved)
		}
		return "", fmt.Errorf("inspect slot path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("slot path is a symlink, refusing: %s", resolved)
	}
	realPath, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve slot path: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return "", fmt.Errorf("resolve pool root: %w", err)
	}
	rel, err := filepath.Rel(filepath.Clean(realRoot), filepath.Clean(realPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("slot path resolves outside the pool root: %s", resolved)
	}
	// Return the symlink-resolved path, not the as-recorded one: a tmpdir or
	// other ancestor symlink (e.g. macOS /var -> /private/var) means git's
	// own worktree registry and this process can disagree on which string
	// names the same directory unless both compare post-resolution.
	return filepath.Clean(realPath), nil
}

// registeredWorktrees returns every worktree git itself has registered for
// this repository, keyed by its normalized absolute path. GC must never
// treat a directory as a removable pool slot unless git independently
// confirms it as an exact, currently-registered worktree.
func registeredWorktrees(ctx context.Context, repoRoot string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	current := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = filepath.Clean(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "HEAD ") && current != "":
			result[current] = strings.TrimPrefix(line, "HEAD ")
		}
	}
	return result, nil
}

// verifyReachableFromBase refuses to treat a slot as safe to destroy unless
// its exact current HEAD is an ancestor of the pool's base ref. A slot whose
// HEAD diverged from base carries commits that may exist nowhere else once
// the worktree is gone; deleting the checkout does not by itself delete
// shared objects, but an unreachable commit becomes unreachable garbage the
// next real gc collects, which is indistinguishable from data loss to
// whoever made it.
func verifyReachableFromBase(ctx context.Context, repoRoot, slotPath, base string) error {
	headOut, err := exec.CommandContext(ctx, "git", "-C", slotPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("resolve slot HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headOut))
	if head == "" {
		return errors.New("slot HEAD is empty")
	}
	if err := exec.CommandContext(ctx, "git", "-C", repoRoot, "merge-base", "--is-ancestor", head, base).Run(); err != nil {
		return fmt.Errorf("slot HEAD %s is not reachable from base %s, refusing to discard possibly-unique work", head, base)
	}
	return nil
}

// gitFullyClean checks working-tree cleanliness including untracked and
// git-ignored content, unlike gitClean's tracked-and-untracked-only check
// used by Lease/Release. GC's bar is stricter: an ignored build artifact a
// reviewer left behind is still that reviewer's content, not the pool's.
func gitFullyClean(ctx context.Context, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain", "--ignored")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git status %s: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "", nil
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
		data, err := os.ReadFile(path) // #nosec G122 -- the path is yielded by the bounded source WalkDir during an internal tree copy.
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
