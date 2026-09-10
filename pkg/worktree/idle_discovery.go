package worktree

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// IdlePoolDiscoveryConfig bounds one tick of idle, non-current pool root
// discovery and reclamation. Discovery never scans anything outside the
// repository's own canonical .herd directory, never recurses into worktree
// contents or history, and never deletes a worktree directly -- every
// removal goes through Pool.GC, the same fail-closed safety primitive used
// by explicit one-off GC.
type IdlePoolDiscoveryConfig struct {
	RepoRoot    string
	DefaultBase string
	// MaxRoots bounds how many candidate pool roots one tick fully inspects
	// (schema check, protection check, GC verification). Defaults to 10.
	MaxRoots int
	// MaxElapsed bounds how long one tick may spend, including the downstream
	// Pool.GC/GCPlan calls it makes. Defaults to 30s.
	MaxElapsed time.Duration
	Now        func() time.Time
	// ManifestPath is the review-retirement launch manifest registry
	// (.herd/review/retirement-manifests.jsonl).
	ManifestPath string
	// PhaseJournalPath is the review-retirement phase journal
	// (.herd/review/retirement-phases.jsonl), the authoritative record of
	// whether a given manifested generation actually completed retirement.
	// A pool root is protected only while at least one of its manifested
	// generations lacks a matching "complete" phase record -- once every
	// generation the manifest ever named for that pool is confirmed
	// complete, the pool is no longer eternally retained. Malformed or
	// unreadable evidence at either path fails the whole tick closed rather
	// than guessing.
	PhaseJournalPath string
	// ProcessInspector overrides the process-ownership census Pool.GC uses.
	// nil means each constructed Pool keeps its own default (the real
	// native lsof-based census from NewPool) -- production discovery never
	// bypasses live-process safety. Tests inject a deterministic fixture.
	ProcessInspector resources.ProcessInspector
}

// IdlePoolStatus is one candidate's disposition for a discovery tick.
type IdlePoolStatus string

const (
	IdlePoolEligible IdlePoolStatus = "eligible"
	IdlePoolRetained IdlePoolStatus = "retained"
	IdlePoolFailed   IdlePoolStatus = "failed"
)

// IdlePoolDisposition is one candidate pool root's exact outcome.
type IdlePoolDisposition struct {
	Root   string         `json:"root"`
	Status IdlePoolStatus `json:"status"`
	Reason string         `json:"reason,omitempty"`
}

// IdlePoolDiscoveryResult is one tick's complete, bounded report.
type IdlePoolDiscoveryResult struct {
	Dispositions []IdlePoolDisposition `json:"dispositions"`
	Scanned      int                   `json:"scanned"`
	Eligible     int                   `json:"eligible"`
	Retained     int                   `json:"retained"`
	Failed       int                   `json:"failed"`
	NextCursor   string                `json:"next_cursor"`
}

const (
	idlePoolDiscoveryCursorFile = "pool-discovery-cursor.json"
	idlePoolDiscoveryLockFile   = "pool-discovery.lock"
)

type idlePoolCursorState struct {
	Last string `json:"last"`
}

func idlePoolCursorPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".herd", idlePoolDiscoveryCursorFile)
}

func idlePoolLockPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".herd", idlePoolDiscoveryLockFile)
}

// withIdlePoolTickLock serializes whole ticks against each other -- reading
// the cursor, deciding candidates, acting, and persisting the cursor all
// happen under one lock, the same O_EXCL pattern Pool itself uses for its
// own state. Without it two concurrent ticks (a manual `herd idle-pool
// --act` racing the daemon's own pulse cadence) could both read the same
// starting cursor and silently discard each other's fairness progress, or
// interleave writes to the cursor file. A concurrent tick returns a plain
// "busy" error rather than corrupting anything.
func withIdlePoolTickLock(repoRoot string, fn func() error) error {
	lockPath := idlePoolLockPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("idle pool discovery: create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("idle pool discovery: tick is busy or lock is stale: %w", err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(lockPath) }()
	return fn()
}

func readIdlePoolCursor(repoRoot string) (string, error) {
	data, err := os.ReadFile(idlePoolCursorPath(repoRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("idle pool discovery: read cursor: %w", err)
	}
	var st idlePoolCursorState
	if err := json.Unmarshal(data, &st); err != nil {
		// A corrupt cursor must not halt discovery -- restart from the top.
		// That costs one tick's worth of fairness, never safety.
		return "", nil
	}
	return st.Last, nil
}

// writeIdlePoolCursor persists progress. It is called only when a tick
// actually acted (act=true): a pure dry-run/observe pass must never mutate
// repository state, cursor included.
func writeIdlePoolCursor(repoRoot, last string) error {
	data, err := json.Marshal(idlePoolCursorState{Last: last})
	if err != nil {
		return err
	}
	path := idlePoolCursorPath(repoRoot)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("idle pool discovery: write cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("idle pool discovery: publish cursor: %w", err)
	}
	return nil
}

// listPoolRootNames lists candidate names directly under <repoRoot>/.herd --
// a single non-recursive syscall, filtered on the DirEntry metadata ReadDir
// already returns (name and directory bit), with no per-entry stat, open,
// or parse. The bare ".herd/pool" (the live, in-rotation pool) is never a
// candidate. Building the full candidate-name universe is unavoidable to
// compute deterministic fair rotation, but it costs nothing beyond the one
// syscall; every per-candidate inspection (schema parse, symlink check,
// protection lookup, GC verification) happens later, only for the bounded
// subset a tick actually visits.
func listPoolRootNames(repoRoot string) ([]string, error) {
	herdDir := filepath.Join(repoRoot, ".herd")
	entries, err := os.ReadDir(herdDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("idle pool discovery: list %s: %w", herdDir, err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name == "pool" || !strings.HasPrefix(name, "pool-") || !e.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// inspectPoolRootSchema performs the per-candidate checks Google's review
// found running unconditionally over every entry before any bound applied:
// symlink rejection and pool.json schema validation. Called only for
// candidates a tick has already decided, within budget, to visit.
func inspectPoolRootSchema(candidatePath string) (ok bool, reason string) {
	info, err := os.Lstat(candidatePath)
	if err != nil {
		return false, fmt.Sprintf("inspect: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, "root is a symlink"
	}
	data, err := os.ReadFile(filepath.Join(candidatePath, "pool.json"))
	if err != nil {
		return false, fmt.Sprintf("no readable pool.json: %v", err)
	}
	var st poolState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != 1 {
		return false, "pool.json does not match the known schema"
	}
	return true, ""
}

type retirementIdentity struct {
	Generation, CandidateSHA, Reviewer, BindingDigest string
}

func (id retirementIdentity) key(pool string) string {
	return pool + "\x00" + id.Generation + "\x00" + id.CandidateSHA + "\x00" + id.Reviewer + "\x00" + id.BindingDigest
}

// scanJSONL calls fn with each non-empty trimmed line of path. A missing
// file is not an error (nothing recorded yet); a read error is.
func scanJSONL(path string, fn func([]byte) error) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// manifestProtectedPools returns the set of pool root names still under
// active or unconfirmed retirement evidence.
//
// A pool root is protected while ANY generation the manifest registry ever
// named for it lacks a matching "complete" record in the retirement phase
// journal -- the authoritative record NativeReviewRetirementOp writes only
// after a real retirement (Observe -> Close -> lease release -> worktree ->
// branch -> artifact -> Receipt) fully finishes for that exact
// generation/candidate/reviewer/binding. Once every manifested generation
// for a pool is confirmed complete, that pool is no longer eternally
// retained -- its evidence is preserved (this scan never touches the
// manifest or phase files), but its now-idle worktrees are eligible again.
//
// Manifests are never ignored (a pool never mentioned there gets no
// protection from this check at all, same as before); what changed is that
// a mention is no longer a permanent veto. Any unparseable line in either
// file fails the whole tick closed: evidence integrity is not this scan's
// to resolve, and protecting nothing is worse than protecting too much when
// completion cannot be verified.
func manifestProtectedPools(manifestPath, phaseJournalPath string) (map[string]bool, error) {
	manifestsByPool := make(map[string][]retirementIdentity)
	if err := scanJSONL(manifestPath, func(line []byte) error {
		var row struct {
			Pool, Generation, CandidateSHA, Reviewer, BindingDigest string
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("idle pool discovery: manifest registry line is unparseable, refusing evidence-protection determination: %w", err)
		}
		name := strings.TrimSpace(filepath.Base(row.Pool))
		if name == "" {
			return nil
		}
		manifestsByPool[name] = append(manifestsByPool[name], retirementIdentity{row.Generation, row.CandidateSHA, row.Reviewer, row.BindingDigest})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(manifestsByPool) == 0 {
		return map[string]bool{}, nil
	}

	completed := make(map[string]bool)
	if err := scanJSONL(phaseJournalPath, func(line []byte) error {
		var rec struct {
			Pool, Generation, CandidateSHA, Reviewer, BindingDigest, Phase string
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("idle pool discovery: retirement phase journal line is unparseable, refusing evidence-completion determination: %w", err)
		}
		if rec.Phase != "complete" {
			return nil
		}
		name := strings.TrimSpace(filepath.Base(rec.Pool))
		if name == "" {
			return nil
		}
		id := retirementIdentity{rec.Generation, rec.CandidateSHA, rec.Reviewer, rec.BindingDigest}
		completed[id.key(name)] = true
		return nil
	}); err != nil {
		return nil, err
	}

	protected := make(map[string]bool)
	for pool, ids := range manifestsByPool {
		for _, id := range ids {
			if !completed[id.key(pool)] {
				protected[pool] = true
				break
			}
		}
	}
	return protected, nil
}

// DiscoverIdlePools reports, without mutating anything -- no reclamation
// and no cursor write -- the exact disposition of up to cfg.MaxRoots
// candidates starting immediately after the durable cursor. It runs the
// identical eligibility check GC itself uses (via Pool.GCPlan), so a dry
// run cannot diverge from a real pass.
func DiscoverIdlePools(ctx context.Context, cfg IdlePoolDiscoveryConfig) (IdlePoolDiscoveryResult, error) {
	return runIdlePoolTick(ctx, cfg, false)
}

// ReclaimIdlePools performs the identical bounded discovery pass and calls
// Pool.GC -- never a raw directory delete -- on every eligible root, then
// persists the fairness cursor.
func ReclaimIdlePools(ctx context.Context, cfg IdlePoolDiscoveryConfig) (IdlePoolDiscoveryResult, error) {
	return runIdlePoolTick(ctx, cfg, true)
}

func runIdlePoolTick(ctx context.Context, cfg IdlePoolDiscoveryConfig, act bool) (IdlePoolDiscoveryResult, error) {
	if strings.TrimSpace(cfg.RepoRoot) == "" {
		return IdlePoolDiscoveryResult{}, errors.New("idle pool discovery: repo root is required")
	}
	var result IdlePoolDiscoveryResult
	err := withIdlePoolTickLock(cfg.RepoRoot, func() error {
		var innerErr error
		result, innerErr = runIdlePoolTickLocked(ctx, cfg, act)
		return innerErr
	})
	return result, err
}

func runIdlePoolTickLocked(ctx context.Context, cfg IdlePoolDiscoveryConfig, act bool) (IdlePoolDiscoveryResult, error) {
	maxRoots := cfg.MaxRoots
	if maxRoots <= 0 {
		maxRoots = 10
	}
	maxElapsed := cfg.MaxElapsed
	if maxElapsed <= 0 {
		maxElapsed = 30 * time.Second
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	deadline := start.Add(maxElapsed)

	names, err := listPoolRootNames(cfg.RepoRoot)
	if err != nil {
		return IdlePoolDiscoveryResult{}, err
	}
	protected, err := manifestProtectedPools(cfg.ManifestPath, cfg.PhaseJournalPath)
	if err != nil {
		return IdlePoolDiscoveryResult{}, err
	}
	cursor, err := readIdlePoolCursor(cfg.RepoRoot)
	if err != nil {
		return IdlePoolDiscoveryResult{}, err
	}

	var result IdlePoolDiscoveryResult
	lastVisited := cursor
	processed := 0
	for _, name := range fairOrder(names, cursor) {
		if processed >= maxRoots || now().Sub(start) >= maxElapsed {
			break
		}
		processed++
		lastVisited = name
		result.Scanned++
		candidatePath := filepath.Join(cfg.RepoRoot, ".herd", name)

		if ok, reason := inspectPoolRootSchema(candidatePath); !ok {
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolFailed, Reason: reason})
			result.Failed++
			continue
		}
		if protected[name] {
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolRetained, Reason: "active or unconfirmed-complete retirement evidence references this pool, protected"})
			result.Retained++
			continue
		}

		pool := NewPool(cfg.RepoRoot, candidatePath, 0)
		pool.DefaultBase = cfg.DefaultBase
		if cfg.Now != nil {
			pool.Now = cfg.Now
		}
		if cfg.ProcessInspector != nil {
			pool.ProcessInspector = cfg.ProcessInspector
		}

		// context.WithDeadline bounds every exec.CommandContext call inside
		// Pool.GCPlan/GC -- the dominant real cost (git status/worktree
		// list/merge-base). It does not preempt a blocked raw syscall
		// (os.Stat/os.ReadFile take no context in Go's stdlib), so this is a
		// real but partial time bound, not a claim of hard preemption over
		// every possible stall.
		planCtx, planCancel := context.WithDeadline(ctx, deadline)
		decisions, planErr := pool.GCPlan(planCtx)
		planCancel()
		if planErr != nil {
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolFailed, Reason: planErr.Error()})
			result.Failed++
			continue
		}
		refused := ""
		for _, d := range decisions {
			if d.Refused {
				refused = d.Reason
				break
			}
		}
		if refused != "" {
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolRetained, Reason: refused})
			result.Retained++
			continue
		}
		if !act {
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolEligible})
			result.Eligible++
			continue
		}

		actCtx, actCancel := context.WithDeadline(ctx, deadline)
		gcErr := pool.GC(actCtx)
		actCancel()
		if gcErr != nil {
			// GCPlan just certified every slot here clear; a GC failure now
			// is a genuine, unexpected operational failure (a race, disk, or
			// git error) rather than an ordinary safety refusal, and must be
			// reported as a real failure so it propagates as non-zero, not
			// folded into the benign retained/leased bucket.
			result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolFailed, Reason: gcErr.Error()})
			result.Failed++
			continue
		}
		result.Dispositions = append(result.Dispositions, IdlePoolDisposition{Root: name, Status: IdlePoolEligible})
		result.Eligible++
	}

	if act {
		if lastVisited != "" {
			if err := writeIdlePoolCursor(cfg.RepoRoot, lastVisited); err != nil {
				return result, err
			}
		}
		result.NextCursor = lastVisited
	} else {
		// A dry run must not mutate durable state, cursor included; report
		// what would become the next cursor without persisting it.
		result.NextCursor = lastVisited
	}
	return result, nil
}

// fairOrder rotates the sorted candidate list to begin immediately after
// cursor, so a tick that cannot cover every candidate still guarantees
// every root is eventually visited rather than always favoring names that
// sort earliest. cursor absent, stale, or past every current name simply
// wraps to the top -- always a safe, well-defined starting point.
func fairOrder(names []string, cursor string) []string {
	if len(names) == 0 {
		return names
	}
	start := len(names)
	for i, n := range names {
		if n > cursor {
			start = i
			break
		}
	}
	out := make([]string, 0, len(names))
	out = append(out, names[start:]...)
	out = append(out, names[:start]...)
	return out
}
