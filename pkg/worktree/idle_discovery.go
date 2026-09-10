package worktree

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// ErrIdlePoolTickBusy reports that a tick could not acquire the discovery
// lock because another live tick holds the kernel flock. Losing this race
// is an expected benign deferral, never a failure.
var ErrIdlePoolTickBusy = errors.New("tick is busy or lock is stale")

const idlePoolTickLockIdentityVersion = 1

// idlePoolTickLockIdentity is the advisory owner/generation record written
// into the tick lock file while its flock is held. The mutex is the kernel
// flock -- released automatically when the holder dies -- so the record
// never decides liveness and never justifies unlinking the path; it exists
// so a human (or a forensic scan) can see who held the lock last.
type idlePoolTickLockIdentity struct {
	Version   int       `json:"version"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// withIdlePoolTickLock serializes whole ticks against each other -- reading
// the cursor, deciding candidates, acting, and persisting the cursor all
// happen under one lock. The mutex is the kernel flock on the open file
// description, never the directory entry: flock is owned by the open file,
// is released atomically by the kernel when the holder exits or crashes, and
// arbitrates two stale contenders by construction -- the second LOCK_EX
// attempt fails with EWOULDBLOCK instead of both contenders agreeing the
// previous owner looked dead and deleting each other's pathname. There is
// deliberately no unlink anywhere in this protocol: a lock file left on disk
// is an inert forensics record, and removing by pathname is exactly how a
// stale holder deletes a replacement owner's live lock. A concurrent tick
// returns ErrIdlePoolTickBusy rather than corrupting anything.
func withIdlePoolTickLock(repoRoot string, fn func() error) error {
	lockPath := idlePoolLockPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("idle pool discovery: create lock dir: %w", err)
	}
	f, err := acquireIdlePoolTickLock(lockPath)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("idle pool discovery: %w: another tick holds the kernel lock", ErrIdlePoolTickBusy)
		}
		return fmt.Errorf("idle pool discovery: acquire tick lock: %w", err)
	}
	// Close releases the flock. The path is never unlinked.
	defer func() { _ = f.Close() }()
	return fn()
}

// acquireIdlePoolTickLock opens the tick-lock file and takes the exclusive
// kernel flock on it. The identity record written under the lock is advisory
// forensics (who held it last); the mutex itself is the flock, so a corrupt,
// version-mismatched, or foreign-host record can never wedge acquisition --
// and must never be used to justify unlinking a path that may name a live
// replacement owner's lock.
func acquireIdlePoolTickLock(lockPath string) (*os.File, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	identity := idlePoolTickLockIdentity{Version: idlePoolTickLockIdentityVersion, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if host, hostErr := os.Hostname(); hostErr == nil {
		identity.Host = host
	}
	data, marshalErr := json.Marshal(identity)
	if marshalErr != nil {
		_ = f.Close()
		return nil, marshalErr
	}
	// Rewrite the record in place under the held flock: truncate first so a
	// stale longer record can never leave trailing bytes behind.
	if _, writeErr := f.WriteAt(append(data, '\n'), 0); writeErr != nil {
		_ = f.Close()
		return nil, writeErr
	}
	if truncErr := f.Truncate(int64(len(data) + 1)); truncErr != nil {
		_ = f.Close()
		return nil, truncErr
	}
	return f, nil
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
// candidate. The enumeration itself is bounded: a .herd directory with more
// than maxPoolRootEntries pool-* entries is a pathological state and the
// tick refuses closed rather than materializing an unbounded name slice;
// every per-candidate inspection (schema parse, symlink check, protection
// lookup, GC verification) still happens later, only for the bounded subset
// a tick actually visits.
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
		if len(names) >= maxPoolRootEntries {
			return nil, fmt.Errorf("idle pool discovery: %s holds more than %d pool roots, refusing unbounded enumeration", herdDir, maxPoolRootEntries)
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
	data, err := readBoundedFile(filepath.Join(candidatePath, "pool.json"), maxPoolJSONBytes)
	if err != nil {
		return false, err.Error()
	}
	var st poolState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != 1 {
		return false, "pool.json does not match the known schema"
	}
	return true, ""
}

// readBoundedFile reads at most maxBytes+1 bytes and refuses a file at or
// over the budget: a pool.json larger than the bound is a pathological or
// hostile state, and truncating it silently could make a parse succeed on
// evidence that is not the evidence on disk.
func readBoundedFile(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("no readable pool.json: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("no readable pool.json: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("pool.json exceeds the %d-byte evidence bound, refusing", maxBytes)
	}
	return data, nil
}

type retirementIdentity struct {
	Generation, CandidateSHA, Reviewer, BindingDigest string
}

func (id retirementIdentity) key(pool string) string {
	return pool + "\x00" + id.Generation + "\x00" + id.CandidateSHA + "\x00" + id.Reviewer + "\x00" + id.BindingDigest
}

// scanJSONL calls fn with each non-empty trimmed line of path. A missing
// file is not an error (nothing recorded yet); a read error, an
// over-length line, or a total byte/record budget overrun is. The budgets
// bound a pathological or hostile evidence file: evidence integrity beyond
// the budget is not this scan's to resolve, and the callers fail the whole
// tick closed.
const (
	maxJSONLTotalBytes = 8 << 20
	maxJSONLRecords    = 65536
	maxJSONLLineBytes  = 1 << 20
	maxPoolJSONBytes   = 1 << 20
	maxPoolRootEntries = 4096
)

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
	var totalBytes, records int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJSONLLineBytes)
	for sc.Scan() {
		totalBytes += len(sc.Bytes()) + 1
		if totalBytes > maxJSONLTotalBytes {
			return fmt.Errorf("read %s: retirement evidence exceeds %d total bytes, refusing", path, maxJSONLTotalBytes)
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		records++
		if records > maxJSONLRecords {
			return fmt.Errorf("read %s: retirement evidence exceeds %d records, refusing", path, maxJSONLRecords)
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// manifestProtectedPools returns two things, keyed by absolute,
// repo-identity-bound pool-root paths, never by basename:
//
//   - protected: pool roots named by ANY manifest generation that lacks a
//     matching "complete" record in the retirement phase journal.
//   - known: pool roots the manifest registry positively names at all.
//
// A pool root absent from the manifest is NOT automatically disposable:
// the destructive authority requires positive evidence, so an unknown or
// unresolvable pool identity retains. All three evidence inputs are
// resolved through the ACTUAL repository root identity (symlink-resolved
// once, here), not the current spelling of a possibly-replaced directory.
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
// Manifests are never ignored; what changed over time is that a mention is
// no longer a permanent veto once completion is provable. Any unparseable
// line in either file fails the whole tick closed: evidence integrity is
// not this scan's to resolve, and protecting nothing is worse than
// protecting too much when completion cannot be verified.
//
// Keying is by absolute pool-root identity: a recorded manifest path is
// resolved against the repository root (absolute forms pass through), and
// both the recorded path itself and its parent are keyed, so a manifest row
// recorded as either a pool root or a slot path protects exactly that pool
// -- and can never misassociate a same-basename pool under a different
// repository root.
func manifestProtectedPools(repoRoot, manifestPath, phaseJournalPath string) (protected map[string]bool, known map[string]bool, err error) {
	// Bind every identity to the actual repository root, symlink-resolved:
	// evidence belongs to the repository and pool that exist on disk under
	// this root, not to a current path spelling that a replacement could
	// have rewritten.
	rootIdentity := filepath.Clean(repoRoot)
	if resolved, resolveErr := filepath.EvalSymlinks(rootIdentity); resolveErr == nil {
		rootIdentity = filepath.Clean(resolved)
	}
	resolvePoolIdentity := func(recorded string) (string, string) {
		p := strings.TrimSpace(filepath.ToSlash(recorded))
		if p == "" {
			return "", ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoRoot, filepath.FromSlash(p))
		}
		p = filepath.Clean(p)
		// Identity is the real filesystem object, not a path spelling: the
		// same directory reachable as /var/x and /private/var/x (or through
		// an ancestor symlink) must resolve to one key. An unresolvable
		// path (gone mid-tick) keeps its cleaned spelling -- which can only
		// over-protect, never under-protect.
		if resolved, resolveErr := filepath.EvalSymlinks(p); resolveErr == nil {
			p = resolved
		}
		return p, filepath.Dir(p)
	}
	manifestsByPool := make(map[string][]retirementIdentity)
	if err := scanJSONL(manifestPath, func(line []byte) error {
		var row struct {
			Pool, Generation, CandidateSHA, Reviewer, BindingDigest string
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("idle pool discovery: manifest registry line is unparseable, refusing evidence-protection determination: %w", err)
		}
		self, parent := resolvePoolIdentity(row.Pool)
		if self == "" {
			return nil
		}
		id := retirementIdentity{row.Generation, row.CandidateSHA, row.Reviewer, row.BindingDigest}
		manifestsByPool[self] = append(manifestsByPool[self], id)
		if parent != self {
			manifestsByPool[parent] = append(manifestsByPool[parent], id)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if len(manifestsByPool) == 0 {
		return map[string]bool{}, map[string]bool{}, nil
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
		self, parent := resolvePoolIdentity(rec.Pool)
		if self == "" {
			return nil
		}
		id := retirementIdentity{rec.Generation, rec.CandidateSHA, rec.Reviewer, rec.BindingDigest}
		completed[id.key(self)] = true
		if parent != self {
			completed[id.key(parent)] = true
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}

	protected = make(map[string]bool)
	known = make(map[string]bool)
	for pool := range manifestsByPool {
		// A pool root that resolves outside the actual repository identity
		// is mismatched scope: it can never be authorized for destruction
		// from this root, and its mention protects it (retain, fail closed).
		if pool != rootIdentity && !strings.HasPrefix(pool, rootIdentity+string(filepath.Separator)) {
			protected[pool] = true
			known[pool] = true
			continue
		}
		known[pool] = true
		for _, id := range manifestsByPool[pool] {
			if !completed[id.key(pool)] {
				protected[pool] = true
				break
			}
		}
	}
	return protected, known, nil
}

// ManifestRetirementAuthority is the production SlotRetirementAuthority: it
// answers from the same manifest registry and phase journal the
// idle-discovery protection scan reads, keyed by absolute pool-root
// identity. Both the direct `herd pool gc` path and the idle-discovery
// caller construct it, so the destructive primitive enforces one
// retirement-evidence fence for every caller.
type ManifestRetirementAuthority struct {
	RepoRoot         string
	ManifestPath     string
	PhaseJournalPath string
}

// NewManifestRetirementAuthority builds the manifest/journal-backed
// authority for a repository root. The evidence paths are supplied by the
// caller (production passes herdr.ReviewRetirementRegistryPath and the
// retirement phase journal path).
func NewManifestRetirementAuthority(repoRoot, manifestPath, phaseJournalPath string) *ManifestRetirementAuthority {
	return &ManifestRetirementAuthority{RepoRoot: repoRoot, ManifestPath: manifestPath, PhaseJournalPath: phaseJournalPath}
}

// AuthorizePoolRoot refuses when the exact pool root is named by unretired
// or unconfirmed retirement evidence, and refuses when the evidence itself
// is unreadable or over-budget (manifestProtectedPools errors fail closed).
// The incoming root is symlink-normalized to the same real-filesystem
// identity the evidence resolution uses, so path spellings can never dodge
// the fence.
func (a *ManifestRetirementAuthority) AuthorizePoolRoot(poolRootAbs string) error {
	if resolved, err := filepath.EvalSymlinks(poolRootAbs); err == nil {
		poolRootAbs = resolved
	}
	poolRootAbs = filepath.Clean(poolRootAbs)
	protected, known, err := manifestProtectedPools(a.RepoRoot, a.ManifestPath, a.PhaseJournalPath)
	if err != nil {
		return err
	}
	if !known[poolRootAbs] {
		return fmt.Errorf("pool root %s has no retirement evidence on file; unknown-scope pools are not automatically disposable, refusing", poolRootAbs)
	}
	if protected[poolRootAbs] {
		return fmt.Errorf("pool root %s is under active or unconfirmed retirement evidence, refusing", poolRootAbs)
	}
	return nil
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
	protected, _, err := manifestProtectedPools(cfg.RepoRoot, cfg.ManifestPath, cfg.PhaseJournalPath)
	if err != nil {
		return IdlePoolDiscoveryResult{}, err
	}
	// The same manifest/journal evidence backs the authority the
	// destructive primitive itself will consult: discovery and GC can never
	// disagree about which pools carry verified retirement evidence.
	authority := NewManifestRetirementAuthority(cfg.RepoRoot, cfg.ManifestPath, cfg.PhaseJournalPath)
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
		if protected[candidatePath] {
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
		decisions, planErr := pool.GCPlan(planCtx, authority)
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
		_, gcErr := pool.GC(actCtx, authority)
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
