package worktree

import (
	"bytes"
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
	// OnDestructiveBoundary is a test seam fired at the destructive
	// boundary -- after every identity and evidence gate has passed and
	// before the verified directory is moved into quarantine. Production
	// leaves it nil. It gives a regression harness a deterministic hook to
	// replace the candidate path exactly where the replacement race
	// otherwise lives, so the protocol's guarantees (the replacement's
	// content survives untouched, the pass refuses, nothing is deleted) can
	// be watched RED against the previous direct-removal behavior and GREEN
	// after the fix, instead of trusting a microscopic timing window.
	OnDestructiveBoundary func(slotPath string) error
	// OnQuarantined is a test seam fired after the verified directory has
	// been moved into quarantine and its identity proven, but before git's
	// repair-and-disposal steps. Production leaves it nil. It deterministically
	// injects disposal failure so the retained-with-bytes reporting path is
	// exercisable without depending on git faults.
	OnQuarantined func(quarantinePath string) error
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

// retireStateFenceProbe is a deterministic test seam invoked at the top of
// RetireExact's state-write fence, before the fence re-reads identity and
// absence. Production leaves it a no-op; only tests in this package install
// a probe, and the fence must detect whatever the probe plants. It is not
// reachable from any CLI path.
var retireStateFenceProbe = func(context.Context, *Pool, string) {}

// worktreeRegistered answers, from git's own registration metadata alone,
// whether the exact absolute path is a registered worktree. The porcelain
// format prefixes each registration with "worktree <abs path>"; the comparison
// is exact and symlink-resolved — git prints the resolved absolute path while
// the caller may hold the unresolved one (/var vs /private/var on macOS) —
// and never a substring match, so a replacement or sibling path cannot read
// as this one.
func (p *Pool) worktreeRegistered(ctx context.Context, slotPath string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("git worktree list --porcelain: %w", err)
	}
	want := canonicalDirPath(slotPath)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		if canonicalDirPath(strings.TrimSpace(strings.TrimPrefix(line, "worktree "))) == want {
			return true, nil
		}
	}
	return false, nil
}

// canonicalDirPath resolves the parent chain so the same directory reachable
// through different symlinks compares equal, while the final element is kept
// verbatim because the leaf itself may be absent.
func canonicalDirPath(path string) string {
	cleaned := filepath.Clean(path)
	parent := filepath.Dir(cleaned)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return cleaned
	}
	return filepath.Join(resolved, filepath.Base(cleaned))
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
			completeSlot := func() error {
				// FAC-807 state-write fence: identity/absence is re-read
				// immediately before the record mutation, so a replacement
				// or re-registration that appeared after the earlier checks
				// (a raw filesystem writer is not serialized by the pool
				// lock) is detected here and the transition refuses. The
				// absent-path completion deletes nothing, so the fence's
				// refusal is always the safe outcome.
				retireStateFenceProbe(ctx, p, slotPath)
				if _, statErr := os.Lstat(slotPath); statErr == nil {
					return fmt.Errorf("worktree pool: slot %s path reappeared before state completion; refusing ambiguous retirement", slot.Name)
				} else if !errors.Is(statErr, fs.ErrNotExist) {
					return fmt.Errorf("worktree pool: fence stat %s: %w", slot.Name, statErr)
				}
				if fenceRegistered, fenceErr := p.worktreeRegistered(ctx, slotPath); fenceErr != nil {
					return fmt.Errorf("worktree pool: fence readback %s: %w", slot.Name, fenceErr)
				} else if fenceRegistered {
					return fmt.Errorf("worktree pool: slot %s registration reappeared before state completion; refusing ambiguous retirement", slot.Name)
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
			registered, readbackErr := p.worktreeRegistered(ctx, slotPath)
			if readbackErr != nil {
				return fmt.Errorf("worktree pool: readback %s: %w", slot.Name, readbackErr)
			}
			if _, statErr := os.Lstat(slotPath); statErr != nil {
				if !errors.Is(statErr, fs.ErrNotExist) {
					return fmt.Errorf("worktree pool: stat %s: %w", slot.Name, statErr)
				}
				// FAC-807: the surface is gone — an earlier exact retirement
				// may already have removed it, or something external did.
				// Completion is authorized ONLY by positive evidence that git
				// no longer registers this exact path; a registered-but-missing
				// path is a corrupt or ambiguous state and must fail closed.
				if registered {
					return fmt.Errorf("worktree pool: slot %s path is absent but still registered; refusing ambiguous retirement", slot.Name)
				}
				return completeSlot()
			}
			if !registered {
				// The path exists but git does not register it — including an
				// EMPTY directory. Emptiness is not proof of ownership: a
				// replacement may have been created after the original
				// worktree disappeared. Absence-only retirement never deletes
				// an existing directory, so this refuses unconditionally.
				return fmt.Errorf("worktree pool: slot %s path holds unregistered content; refusing possible replacement", slot.Name)
			}
			cmd := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "remove", "--force", slotPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree pool: remove %s: %v (%s)", slot.Name, err, strings.TrimSpace(string(out)))
			}
			if err := os.RemoveAll(slotPath); err != nil {
				return err
			}
			return completeSlot()
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
// SlotRetirementAuthority is the positive-evidence fence the destructive GC
// primitive requires: before any slot is removed, the authority must affirm
// that the slot's pool root carries verified retirement evidence. A nil
// authority or an error answer refuses GC -- the protection lives in the
// destructive primitive itself, not in any particular caller, so a direct
// `herd pool gc` can never bypass the retirement-evidence fence the
// idle-discovery caller enforces.
type SlotRetirementAuthority interface {
	// AuthorizePoolRoot answers whether the exact pool root (an absolute
	// path) is authorized for destructive reclamation. Errors refuse.
	AuthorizePoolRoot(poolRootAbs string) error
}

// GCReclaimReport is the per-slot readback of one destructive GC pass. A
// successful reclaim carries the payload bytes actually measured and
// verified gone from disk; a retained or pending slot carries the bytes
// preserved on disk and where they are parked, never a silent success.
type GCReclaimReport struct {
	Slot           string
	Path           string
	Removed        bool
	ReclaimedBytes int64
	ParkedAt       string
	Reason         string
}

// maxPayloadWalkBytes bounds the payload measurement walk. A slot whose
// payload exceeds it is retained (refused) rather than reclaimed
// unmeasured.
const maxPayloadWalkBytes = 1 << 30

// walkPayload measures the on-disk payload of a slot directory with a hard
// budget. The measurement is what the reclaim readback is held against:
// success must prove exactly this many bytes were actually deleted.
func walkPayload(root string, maxBytes int64) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxBytes {
			return fmt.Errorf("payload exceeds the %d-byte measurement bound, refusing to reclaim unmeasured", maxBytes)
		}
		return nil
	})
	return total, err
}

// GC removes every slot the verification proves safe and returns the
// per-slot reclaim readback. The authority must be non-nil; absent or
// ambiguous retirement evidence refuses the whole pass rather than guessing
// safe. A nil error never hides retained bytes: every slot whose payload
// was not verifiably deleted reports Removed=false with its bytes and
// parking location preserved.
func (p *Pool) GC(ctx context.Context, authority SlotRetirementAuthority) ([]GCReclaimReport, error) {
	if authority == nil {
		return nil, errors.New("worktree pool: gc refused without a retirement-evidence authority")
	}
	var reports []GCReclaimReport
	err := p.withLock(func() error {
		reports = nil
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
			report := GCReclaimReport{Slot: slot.Name, Path: candidate.path}
			reports = append(reports, report)
			reportIndex := len(reports) - 1
			// The retirement-evidence fence is enforced here, at the single
			// destructive boundary, for every caller alike.
			poolRootAbs := filepath.Dir(candidate.path)
			if err := authority.AuthorizePoolRoot(poolRootAbs); err != nil {
				reports[reportIndex].Reason = err.Error()
				return fmt.Errorf("worktree pool: gc refused for slot %s: %w", slot.Name, err)
			}
			// Re-verify this one slot immediately before removing it: the
			// full-set pass above proves nothing about state a moment later.
			latest, err := p.verifyGCCandidate(ctx, slot)
			if err != nil {
				reports[reportIndex].Reason = err.Error()
				return fmt.Errorf("worktree pool: gc refused for slot %s immediately before removal: %w", slot.Name, err)
			}
			if err := candidate.sameIdentity(latest); err != nil {
				reports[reportIndex].Reason = err.Error()
				return fmt.Errorf("worktree pool: gc refused for slot %s immediately before removal: %w", slot.Name, err)
			}
			// Test seam at the destructive boundary: production leaves it
			// nil. It exists so the replacement race between identity
			// verification and destruction is deterministically exercisable.
			if p.OnDestructiveBoundary != nil {
				if err := p.OnDestructiveBoundary(candidate.path); err != nil {
					reports[reportIndex].Reason = err.Error()
					return fmt.Errorf("worktree pool: gc refused for slot %s at the destructive boundary: %w", slot.Name, err)
				}
			}
			// Measure the payload before anything moves: the reclaim
			// readback is held against exactly this many bytes, and every
			// retained/pending report names the bytes it preserved.
			payloadBytes, measureErr := walkPayload(candidate.path, maxPayloadWalkBytes)
			if measureErr != nil {
				reports[reportIndex].Reason = measureErr.Error()
				return fmt.Errorf("worktree pool: gc refused for slot %s: %w", slot.Name, measureErr)
			}
			reports[reportIndex].ReclaimedBytes = payloadBytes
			// Destructive protocol, using git's own exact move/removal
			// contract -- never a repository-wide prune, never a direct
			// RemoveAll: the verified directory is atomically renamed into
			// a private quarantine under the same pool directory (one
			// syscall, same filesystem -- exclusive ownership of the exact
			// inode just verified and measured), the quarantine entry is
			// proven to be that same inode, and then the registration is
			// moved to the quarantine path with `git worktree repair` and
			// the payload deleted by `git worktree remove` itself, which
			// removes the registered directory it just accepted. If the
			// quarantined identity does not match -- the path was replaced
			// between the gate and the move, so the racer's directory was
			// moved instead -- the move is rolled back atomically and the
			// pass refuses: the replacement's content survives untouched
			// and git metadata stays consistent. A crash at any point
			// leaves the payload parked in quarantine and the slot path
			// absent, which the next pass refuses (retained, bytes
			// preserved); recovery of parked quarantine dirs is a
			// root-owned action.
			quarantine := filepath.Join(p.Root, fmt.Sprintf(".gc-quarantine-%s-%d", slot.Name, p.Now().UnixNano()))
			if err := os.Rename(candidate.path, quarantine); err != nil {
				reports[reportIndex].Reason = fmt.Sprintf("quarantine move failed: %v", err)
				return fmt.Errorf("worktree pool: gc refused for slot %s: quarantine move failed: %w", slot.Name, err)
			}
			rollback := func() error {
				if restoreErr := os.Rename(quarantine, candidate.path); restoreErr != nil {
					reports[reportIndex].ParkedAt = quarantine
					reports[reportIndex].Reason = fmt.Sprintf("rollback failed (%v); %d bytes parked at %s", restoreErr, payloadBytes, quarantine)
					return fmt.Errorf("worktree pool: slot %s rollback failed (%v); %d bytes parked at %s for root recovery", slot.Name, restoreErr, payloadBytes, quarantine)
				}
				return nil
			}
			quarantined, statErr := os.Lstat(quarantine)
			if statErr != nil || !os.SameFile(candidate.info, quarantined) {
				// The verified directory was replaced between the gate and
				// the move; give the replacement its directory back, byte
				// for byte, and refuse.
				if err := rollback(); err != nil {
					return err
				}
				reports[reportIndex].Reason = "path was replaced at the destructive boundary; replacement preserved in place"
				return fmt.Errorf("worktree pool: gc refused for slot %s: path was replaced at the destructive boundary; replacement preserved in place", slot.Name)
			}
			// Test seam inside quarantine, after identity verification and
			// before git disposal: the deterministic injection point for
			// disposal failure (production leaves it nil).
			if p.OnQuarantined != nil {
				if err := p.OnQuarantined(quarantine); err != nil {
					if rbErr := rollback(); rbErr != nil {
						return rbErr
					}
					reports[reportIndex].Reason = fmt.Sprintf("quarantine disposal failed: %v", err)
					return fmt.Errorf("worktree pool: gc retained slot %s with %d bytes on disk: quarantine disposal failed: %w", slot.Name, payloadBytes, err)
				}
			}
			// Adopt the moved registration: git re-points its own metadata
			// at the quarantine path -- an exact, scoped operation on this
			// one worktree, never repository-wide.
			repair := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "repair", quarantine)
			out, repairErr := boundedCombinedOutput(repair, maxGitOutputBytes)
			if repairErr != nil {
				if err := rollback(); err != nil {
					return err
				}
				// The repair moved git's pointer; moving the directory back
				// left it stale again -- re-point it at the original path.
				back := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "repair", candidate.path)
				if _, backErr := boundedCombinedOutput(back, maxGitOutputBytes); backErr != nil {
					reports[reportIndex].ParkedAt = quarantine
					reports[reportIndex].Reason = fmt.Sprintf("registration re-point failed: %v; %d bytes at %s", backErr, payloadBytes, quarantine)
					return fmt.Errorf("worktree pool: slot %s registration re-point failed (%v); %d bytes parked at %s", slot.Name, backErr, payloadBytes, quarantine)
				}
				reports[reportIndex].Reason = fmt.Sprintf("worktree repair failed: %v (%s)", repairErr, strings.TrimSpace(out))
				return fmt.Errorf("worktree pool: gc retained slot %s with %d bytes on disk: worktree repair failed: %v (%s)", slot.Name, payloadBytes, repairErr, strings.TrimSpace(out))
			}
			// git's own removal contract deletes the registered directory
			// it just accepted -- the destructive step is git's, on the
			// quarantine path this pass exclusively owns.
			dispose := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "remove", quarantine)
			out, disposeErr := boundedCombinedOutput(dispose, maxGitOutputBytes)
			if disposeErr != nil {
				// Roll the registration back to the original path, restore
				// the directory, and report a retained slot -- bytes
				// preserved, never a silent success.
				if err := rollback(); err != nil {
					return err
				}
				back := exec.CommandContext(ctx, "git", "-C", p.RepoRoot, "worktree", "repair", candidate.path)
				if _, backErr := boundedCombinedOutput(back, maxGitOutputBytes); backErr != nil {
					reports[reportIndex].ParkedAt = quarantine
					reports[reportIndex].Reason = fmt.Sprintf("disposal failed (%v) and registration re-point failed (%v); %d bytes at %s", disposeErr, backErr, payloadBytes, quarantine)
					return fmt.Errorf("worktree pool: slot %s disposal failed (%v) and re-point failed (%v); %d bytes parked at %s", slot.Name, disposeErr, backErr, payloadBytes, quarantine)
				}
				reports[reportIndex].Reason = fmt.Sprintf("payload disposal failed: %v (%s)", disposeErr, strings.TrimSpace(out))
				return fmt.Errorf("worktree pool: gc retained slot %s with %d bytes on disk: payload disposal failed: %v (%s)", slot.Name, payloadBytes, disposeErr, strings.TrimSpace(out))
			}
			// Readback: success requires the quarantined payload to be
			// verifiably gone from disk. Anything else is a pending state,
			// reported as such -- never success.
			if _, statErr := os.Lstat(quarantine); statErr == nil {
				reports[reportIndex].ParkedAt = quarantine
				reports[reportIndex].Reason = fmt.Sprintf("payload still on disk after git removal; %d bytes parked at %s", payloadBytes, quarantine)
				return fmt.Errorf("worktree pool: slot %s payload still on disk after git removal; %d bytes parked at %s", slot.Name, payloadBytes, quarantine)
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				reports[reportIndex].ParkedAt = quarantine
				reports[reportIndex].Reason = fmt.Sprintf("inspect quarantined payload after git removal: %v", statErr)
				return fmt.Errorf("worktree pool: inspect slot %s after git removal: %w", slot.Name, statErr)
			}
			// The original path must not have been re-occupied by a
			// replacement mid-pass; if it was, preserve it and fail closed
			// rather than recording a clean reclaim over foreign content.
			if info, statErr := os.Lstat(candidate.path); statErr == nil {
				if !os.SameFile(candidate.info, info) {
					reports[reportIndex].Reason = "a replacement occupied the original path after payload disposal; preserved"
					return fmt.Errorf("worktree pool: slot %s path was replaced after payload disposal; preserving it", slot.Name)
				}
				reports[reportIndex].Reason = "slot path remained after payload disposal; preserving it"
				return fmt.Errorf("worktree pool: slot %s remained after payload disposal; preserving it", slot.Name)
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				reports[reportIndex].Reason = fmt.Sprintf("inspect slot path after disposal: %v", statErr)
				return fmt.Errorf("worktree pool: inspect slot %s after payload disposal: %w", slot.Name, statErr)
			}
			removed[i] = true
			reports[reportIndex].Removed = true
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
	return reports, err
}

// GCPlan reports, without deleting anything, which currently-tracked slots
// GC would remove and which it would refuse, and why. It runs the identical
// verification GC uses -- the retirement-evidence authority included -- so a
// dry run cannot diverge from the real decision. A nil authority refuses
// every slot.
func (p *Pool) GCPlan(ctx context.Context, authority SlotRetirementAuthority) ([]GCDecision, error) {
	var decisions []GCDecision
	err := p.withLock(func() error {
		state, err := p.readState()
		if err != nil {
			return err
		}
		for _, slot := range state.Slots {
			d := GCDecision{Slot: slot.Name, Path: slot.Path}
			switch {
			case authority == nil:
				d.Refused = true
				d.Reason = "no retirement-evidence authority provided"
			case slot.LeaseID != "":
				d.Refused = true
				d.Reason = "leased"
			default:
				if err := authority.AuthorizePoolRoot(filepath.Dir(p.repoPath(slot.Path))); err != nil {
					d.Refused = true
					d.Reason = err.Error()
				} else if _, err := p.verifyGCCandidate(ctx, slot); err != nil {
					d.Refused = true
					d.Reason = err.Error()
				}
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

// maxGitOutputBytes bounds the output GC accepts from a single git
// subprocess. A registered-worktree listing or cleanliness report larger
// than this is a pathological state the verification refuses rather than
// silently truncating evidence.
const maxGitOutputBytes = 4 << 20

// boundedCombinedOutput runs cmd and captures at most maxBytes of combined
// output; exceeding the bound fails the call closed.
func boundedCombinedOutput(cmd *exec.Cmd, maxBytes int) (string, error) {
	var buf limitedBuffer
	buf.max = maxBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	if buf.exceeded {
		return buf.String(), fmt.Errorf("subprocess output exceeded %d bytes", maxBytes)
	}
	return buf.String(), runErr
}

type limitedBuffer struct {
	bytes.Buffer
	max      int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max > 0 && b.Len()+len(p) > b.max {
		b.exceeded = true
		b.Buffer.Write(p[:max(0, b.max-b.Len())])
		return len(p), nil // keep the child's stdout pipe draining, then fail closed
	}
	return b.Buffer.Write(p)
}

// registeredWorktrees returns every worktree git itself has registered for
// this repository, keyed by its normalized absolute path. GC must never
// treat a directory as a removable pool slot unless git independently
// confirms it as an exact, currently-registered worktree.
func registeredWorktrees(ctx context.Context, repoRoot string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err := boundedCombinedOutput(cmd, maxGitOutputBytes)
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
	out, err := boundedCombinedOutput(cmd, maxGitOutputBytes)
	if err != nil {
		return false, fmt.Errorf("git status %s: %v (%s)", path, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) == "", nil
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
