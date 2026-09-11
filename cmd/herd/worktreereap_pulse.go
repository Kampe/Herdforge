package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// FAC-805 (pulse side): `herd worktree-reap` is a SWEEP that never had a
// scheduled caller. cmd/herd/main.go dispatches it from the CLI and nothing
// else in cmd/ or pkg/ ever calls it, so between manual runs every landed
// worktree stays registered -- 578 registrations were live when this was
// written, many already at zero commits ahead of origin/main. pkg/pulse
// observes the same leak and only PRINTS advice ("herd worktree-reap --apply
// retires N landed worktree(s)"); no code consumes that string.
//
// This file adds the schedule and nothing else. It introduces no new
// classification and no new removal authority: selection is
// listWorktreeRegistrations + classifyReapEntries and removal is retireLanded,
// the same primitives `herd worktree-reap --apply` uses, carrying every guard
// they already have -- act-time head identity, cleanliness including ignored
// and untracked evidence, lock state, resident-home protection, the owner
// census, and the FAC-805 whole-range landing recheck. The only new decisions
// here are WHEN to look and HOW MUCH to do per beat.
//
// The shape is deliberately the idle-pool precedent (reclaimIdlePoolsOnPulse
// in cmd/herd/idlepool.go, pkg/worktree/idle_discovery.go): one kernel flock
// per tick so two cleanup beats never overlap, a persisted cursor so each beat
// visits a different slice of the fleet, and a hard per-beat budget.
const (
	// reapPulseInspectWindow bounds how many registrations one beat pays a
	// status call for. The whole-fleet status sweep is what makes a reap
	// expensive, and a beat that re-ran it every tick would cost more than the
	// leak: registrations are read with ONE git call that inspects nothing,
	// the classes that can never be landed are dropped from that metadata
	// alone, and only this many survivors are ever inspected.
	reapPulseInspectWindow = 8

	// reapPulseRetireBudget bounds removals per beat, deliberately far below
	// the window. retireLanded's act-time revalidation currently re-lists and
	// re-statuses the WHOLE fleet once per target (see
	// retireLandedOneWithInspectorCensus in cmd/herd/worktreereap.go), so each
	// removal is expensive until that cost is addressed in its own file. A
	// small budget on the pulse cadence drains the leak steadily and can never
	// turn one beat into a fleet-wide stampede.
	reapPulseRetireBudget = 2

	reapPulseCursorFile          = "worktree-reap-pulse.cursor"
	reapPulseLockFile            = "worktree-reap-pulse.lock"
	reapPulseLockIdentityVersion = 1
)

// reapPulseBaseRef resolves the base the beat asks "did this land" against,
// from the repository's own configuration rather than a hardcoded branch, the
// same way the resource governor derives its BaseRef. An unreadable or
// unconfigured project falls back to the repository default. If the resulting
// ref cannot be resolved at all, the native classifier reports "unknown" and
// every affected worktree is kept -- an unanswerable base never reads as
// landed.
func reapPulseBaseRef(root string) string {
	branch := "main"
	if cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml")); err == nil && cfg != nil {
		if configured := strings.TrimSpace(cfg.Project.DefaultBranch); configured != "" {
			branch = configured
		}
	}
	return "origin/" + branch
}

// reapPulseMainWorktreePath resolves the repository's OWN checkout, which is
// never a retirement candidate.
//
// listWorktreeRegistrations marks IsMain by comparing each registration
// against the directory the beat runs FROM, so a beat started inside a linked
// worktree sees the real main checkout as an ordinary registration whose
// branch is, by definition, fully landed. Git refuses to remove a main working
// tree, but a beat that runs on a schedule must not be leaning on that
// refusal. An unresolvable common dir returns "", which excludes nothing and
// leaves the existing IsMain check as the only filter.
//
// The common dir comes from gitroot.CommonDir, the repository's ONE definition
// of that lookup (FAC-575): a second copy here would be exactly the duplicated
// rule that gate exists to prevent, and gitroot already normalizes the
// relative answer older git can return despite --path-format=absolute.
func reapPulseMainWorktreePath(ctx context.Context, root string) string {
	common, err := gitroot.CommonDir(ctx, root)
	if err != nil {
		return ""
	}
	// <main checkout>/.git -> <main checkout>
	main := filepath.Dir(filepath.Clean(common))
	if resolved, err := filepath.EvalSymlinks(main); err == nil {
		main = resolved
	}
	return main
}

// reapPulseRetirer is the removal seam. It defaults to the one native
// retirement authority; tests substitute a recorder so a protection, budget,
// or dry-run assertion never depends on a real removal having happened.
var reapPulseRetirer = retireLanded

// errReapPulseTickBusy reports that another cleanup beat holds the tick lock.
// Losing that race is an expected benign deferral, never a failure: the other
// beat is doing the work.
var errReapPulseTickBusy = errors.New("another cleanup beat holds the tick lock")

// reapPulseReport is one beat's disposition. Counts are reported rather than
// inferred so an operator can see the beat did bounded work, not a sweep.
type reapPulseReport struct {
	Registered int
	Eligible   int
	Inspected  int
	Landed     int
	Retired    int
	Failed     int
	NextCursor string
	Acted      bool
}

// reapLandedWorktreesOnPulse is the scheduled caller worktree-reap never had.
// It rides `herd pulse --act`, which is the one path that actually fires on a
// cadence: the daemon runs it every tick (cmd/herd/main.go runDaemon) and the
// host pulse-beat timer runs it independently of any coordinator.
//
// The disposition contract matches the idle-pool hook exactly. A beat that
// kept everything, or that deferred to a concurrent beat, is a success; only a
// genuine failure returns false, because silently swallowing a real cleanup
// failure would violate this repo's fail-closed invariant.
func reapLandedWorktreesOnPulse(ctx context.Context, errOut *os.File) bool {
	root := canonicalRepoRoot(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "."))
	report, err := runReapPulseTick(ctx, root, reapPulseBaseRef(root), true)
	if err != nil {
		if errors.Is(err, errReapPulseTickBusy) {
			fmt.Fprintf(errOut, "pulse: worktree reap deferred: %v\n", err)
			return true
		}
		fmt.Fprintf(errOut, "pulse: worktree reap: %v\n", err)
		return false
	}
	if report.Inspected > 0 || report.Retired > 0 || report.Failed > 0 {
		fmt.Fprintf(errOut, "pulse: worktree reap registered=%d eligible=%d inspected=%d landed=%d retired=%d failed=%d\n",
			report.Registered, report.Eligible, report.Inspected, report.Landed, report.Retired, report.Failed)
	}
	return report.Failed == 0
}

// runReapPulseTick performs one bounded cleanup beat under the tick lock.
func runReapPulseTick(ctx context.Context, root, base string, act bool) (reapPulseReport, error) {
	var report reapPulseReport
	err := withReapPulseTickLock(root, func() error {
		var tickErr error
		report, tickErr = reapPulseTickLocked(ctx, root, base, act)
		return tickErr
	})
	return report, err
}

// reapPulseTickLocked is the beat itself: cheap listing, cheap exclusion,
// cursor-ordered window, bounded inspection, native classification, bounded
// removal, then cursor advance. Every step before the removal is read-only.
func reapPulseTickLocked(ctx context.Context, root, base string, act bool) (reapPulseReport, error) {
	report := reapPulseReport{Acted: act}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	// ONE git call. Registration metadata only: this inspects no worktree.
	registrations, err := reapRegistrationLister(root)
	if err != nil {
		return report, err
	}
	report.Registered = len(registrations)

	eligible := reapPulseEligible(registrations, reapPulseMainWorktreePath(ctx, root))
	report.Eligible = len(eligible)
	if len(eligible) == 0 {
		return report, nil
	}

	cursor, err := readReapPulseCursor(root)
	if err != nil {
		return report, err
	}
	window := reapPulseFairOrder(eligible, cursor)
	if len(window) > reapPulseInspectWindow {
		window = window[:reapPulseInspectWindow]
	}

	// Cancellation checkpoint before the expensive pass: a beat whose pulse has
	// already been cancelled must not spend the fleet's status budget.
	if err := ctx.Err(); err != nil {
		return report, err
	}
	// The expensive status pass runs ONLY for this window, never the fleet.
	inspected, err := reapEntryInspector(window)
	if err != nil {
		return report, err
	}
	report.Inspected = len(inspected)

	// The native classifier decides. byPR is false: a scheduled beat must not
	// depend on network reachability of a PR host to answer "did this land".
	landed, _ := classifyReapEntries(root, base, false, inspected)
	report.Landed = len(landed)
	if len(landed) > reapPulseRetireBudget {
		landed = landed[:reapPulseRetireBudget]
	}

	if !act {
		// Observe-only leaves the repository byte-identical, cursor included.
		return report, nil
	}
	// The last cancellation checkpoint, and the one that matters: removal is
	// the only destructive step, so a cancelled beat abandons its queued
	// retirements here rather than starting work whose caller has already gone
	// away. The batch itself is not interrupted once begun -- the act-time
	// owner census is batched for the whole set (FAC-809), and tearing a
	// retirement in half is worse than finishing a bounded one.
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if len(landed) > 0 {
		retired, failed := reapPulseRetirer(root, landed)
		report.Retired, report.Failed = len(retired), len(failed)
	}
	// Advance past everything this beat inspected, not merely what it retired:
	// a kept worktree that is kept for a durable reason (unmerged work, a
	// resident home) must not pin the cursor and starve the rest of the fleet.
	if len(window) > 0 {
		report.NextCursor = window[len(window)-1].Path
		if err := writeReapPulseCursor(root, report.NextCursor); err != nil {
			return report, err
		}
	}
	return report, nil
}

// reapPulseEligible drops, from REGISTRATION METADATA ALONE, every class
// classifyReapEntries can never call landed: the repository's own checkout, a
// detached review-pool surface (the pool reclaims those, not this), a locked
// surface, a registration with no branch, and a standing lane's resident home.
// None of those needs a status call, so a beat never spends its window on a
// worktree whose answer is already known.
// mainPath additionally excludes the repository's own checkout by identity,
// for a beat that was started from inside a linked worktree; an empty mainPath
// excludes nothing.
func reapPulseEligible(entries []worktreeEntry, mainPath string) []worktreeEntry {
	out := make([]worktreeEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsMain || entry.Detached || entry.Locked || entry.Branch == "" {
			continue
		}
		if mainPath != "" && reapPulseSamePath(entry.Path, mainPath) {
			continue
		}
		if isResidentHome(entry.Branch, entry.Path) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func reapPulseSamePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// reapPulseFairOrder rotates the candidates so the beat resumes after the last
// path the previous beat inspected. The order is sorted first: the cursor is a
// path comparison, and git lists registrations in registration order, which
// changes as worktrees are added and removed.
func reapPulseFairOrder(entries []worktreeEntry, cursor string) []worktreeEntry {
	ordered := make([]worktreeEntry, len(entries))
	copy(ordered, entries)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	start := len(ordered)
	for i, entry := range ordered {
		if entry.Path > cursor {
			start = i
			break
		}
	}
	out := make([]worktreeEntry, 0, len(ordered))
	out = append(out, ordered[start:]...)
	out = append(out, ordered[:start]...)
	return out
}

func reapPulseStatePath(root, name string) string {
	return filepath.Join(root, ".herd", name)
}

// reapPulseLockIdentity is the advisory record written into the lock file
// while its flock is held. The mutex is the kernel flock, so this record never
// decides liveness and never justifies unlinking the path; it exists so a
// human can see who held the lock last.
type reapPulseLockIdentity struct {
	Version   int       `json:"version"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// withReapPulseTickLock serializes cleanup beats against each other. The mutex
// is the kernel flock on the open file description -- released by the kernel
// when the holder exits or crashes -- and the path is NEVER unlinked, so a
// stale contender can never delete a live owner's lock. A beat that loses the
// race defers immediately; it does not block and does not fail.
//
// This repeats the idle-pool tick-lock shape rather than sharing it: that
// helper is unexported in pkg/worktree and this change is scoped to cmd/herd.
// The two locks are deliberately independent -- pool reclamation and worktree
// retirement touch different surfaces and must not block each other.
func withReapPulseTickLock(root string, fn func() error) error {
	lockPath := reapPulseStatePath(root, reapPulseLockFile)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("worktree reap pulse: create lock dir: %w", err)
	}
	lock, err := acquireReapPulseTickLock(lockPath)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("worktree reap pulse: %w", errReapPulseTickBusy)
		}
		return fmt.Errorf("worktree reap pulse: acquire tick lock: %w", err)
	}
	// Close releases the flock. The path is never unlinked.
	defer func() { _ = lock.Close() }()
	return fn()
}

func acquireReapPulseTickLock(lockPath string) (*os.File, error) {
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, err
	}
	identity := reapPulseLockIdentity{Version: reapPulseLockIdentityVersion, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if host, hostErr := os.Hostname(); hostErr == nil {
		identity.Host = host
	}
	data, marshalErr := json.Marshal(identity)
	if marshalErr != nil {
		_ = lock.Close()
		return nil, marshalErr
	}
	// Rewrite in place under the held flock, truncating so a longer stale
	// record can never leave trailing bytes behind.
	if _, writeErr := lock.WriteAt(append(data, '\n'), 0); writeErr != nil {
		_ = lock.Close()
		return nil, writeErr
	}
	if truncErr := lock.Truncate(int64(len(data) + 1)); truncErr != nil {
		_ = lock.Close()
		return nil, truncErr
	}
	return lock, nil
}

type reapPulseCursorState struct {
	Last string `json:"last"`
}

// readReapPulseCursor returns the cursor as an absolute path for comparison
// against observed registrations. The persisted form is relative to the
// repository root (see writeReapPulseCursor), so a checkout that moves keeps
// its rotation instead of silently restarting.
func readReapPulseCursor(root string) (string, error) {
	data, err := os.ReadFile(reapPulseStatePath(root, reapPulseCursorFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("worktree reap pulse: read cursor: %w", err)
	}
	var state reapPulseCursorState
	if err := json.Unmarshal(data, &state); err != nil {
		// A corrupt cursor restarts the rotation from the top. That costs one
		// beat's worth of fairness, never safety.
		return "", nil
	}
	if strings.TrimSpace(state.Last) == "" {
		return "", nil
	}
	return filepath.Clean(filepath.Join(root, filepath.FromSlash(state.Last))), nil
}

// writeReapPulseCursor persists progress, and is called only by a beat that
// acted: an observe-only pass must not mutate repository state, cursor
// included.
//
// The path is stored RELATIVE to the repository root. An absolute path in
// durable state pins this repository to one machine's directory layout, which
// this repo forbids on sight, and would make every registration compare
// unequal after a move or a remount.
func writeReapPulseCursor(root, last string) error {
	relative, relErr := filepath.Rel(root, last)
	if relErr != nil {
		return fmt.Errorf("worktree reap pulse: cursor is not relative to the repository root: %w", relErr)
	}
	data, err := json.Marshal(reapPulseCursorState{Last: filepath.ToSlash(relative)})
	if err != nil {
		return err
	}
	path := reapPulseStatePath(root, reapPulseCursorFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("worktree reap pulse: write cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("worktree reap pulse: publish cursor: %w", err)
	}
	return nil
}
