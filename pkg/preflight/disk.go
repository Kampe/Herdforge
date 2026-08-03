package preflight

// Disk-pressure guard (FAC-153). Consulted before any fleet mutation
// (worktree create, dispatch/launch, archive/integration): when free bytes,
// free percent, or free inodes on any involved volume fall below configurable
// reserve thresholds, the guard FAILS CLOSED with structured BLOCKED
// disk_pressure evidence. An unreadable disk stat also fails closed.
//
// Origin: live incident 2026-08-02 — shared volume at 99% capacity /
// ~13-18 GiB free while seven task lanes plus review mutation worktrees were
// active; a disk-full error can corrupt partial artifacts and strand
// worktrees, and manual pressure "relief" force-removed 35 sibling worktrees
// without per-target dirty checks.
//
// The guard NEVER deletes, prunes, resets, moves, or cleans anything. It only
// refuses new mutations. Reclamation goes through the safe-GC contract.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// Env-configurable reserve thresholds. Zero disables that axis.
// Defaults chosen so the 2026-08-02 incident state (13-18 GiB free, ~1.4%
// free) is refused: block below 15 GiB free OR below 2% free.
const (
	EnvDiskMinFreeGB      = "HERD_DISK_MIN_FREE_GB"
	EnvDiskMinFreePct     = "HERD_DISK_MIN_FREE_PCT"
	EnvDiskMinInodePct    = "HERD_DISK_MIN_INODE_PCT"
	EnvDiskRecoverFreeGB  = "HERD_DISK_RECOVER_FREE_GB"
	EnvDiskRecoverFreePct = "HERD_DISK_RECOVER_FREE_PCT"
	// EnvDiskBuildHeadroomGB is extra reserve REQUIRED ON TOP of the free
	// floor before a mutation is allowed: expected temp/build expansion
	// (git object writes, race binaries, archives). Defaults to 2GiB; the
	// derived default is off only when every reserve floor is zeroed. Also
	// used as the per-mutation reservation size by Admit.
	EnvDiskBuildHeadroomGB = "HERD_DISK_BUILD_HEADROOM_GB"
	// Soft (serialize) floors: below these but above the block floor,
	// mutation fan-out must drop to serial before any work is refused.
	// Defaults: 2x the effective block floor / 2x the percent floor.
	EnvDiskSerializeFreeGB  = "HERD_DISK_SERIALIZE_FREE_GB"
	EnvDiskSerializeFreePct = "HERD_DISK_SERIALIZE_FREE_PCT"

	defaultDiskMinFreeGB   = 15.0
	defaultDiskMinFreePct  = 2.0
	defaultDiskMinInodePct = 1.0
	// Conservative default temp/build expansion (git object writes, race
	// binaries, archives) required on top of the reserve. Overridable via
	// HERD_DISK_BUILD_HEADROOM_GB; auto-disabled only when every reserve
	// floor is explicitly zeroed (guard wholly disabled, e.g. test shims).
	defaultDiskBuildHeadroomGB = 2.0
	// Hysteresis: once blocked, recover only after a fresh probe shows
	// headroom above recoverFactor x the block threshold.
	recoverFactor = 1.25

	bytesPerGiB = float64(1 << 30)
)

// DiskStat is one volume's observed capacity headroom. Path is
// process-local only (json:"-"): persisted evidence carries the bounded
// Volume label plus FSID/values, never absolute host paths.
type DiskStat struct {
	Path         string  `json:"-"`
	Volume       string  `json:"volume"`
	FSID         string  `json:"fsid"`
	TotalBytes   uint64  `json:"total_bytes"`
	FreeBytes    uint64  `json:"free_bytes"`
	FreePct      float64 `json:"free_pct"`
	TotalInodes  uint64  `json:"total_inodes"`
	FreeInodes   uint64  `json:"free_inodes"`
	InodeFreePct float64 `json:"inode_free_pct"`
}

// DiskThresholds are the effective reserve floors for one check.
type DiskThresholds struct {
	MinFreeBytes     uint64  `json:"min_free_bytes"`
	MinFreePct       float64 `json:"min_free_pct"`
	MinInodePct      float64 `json:"min_inode_pct"`
	RecoverFreeBytes uint64  `json:"recover_free_bytes"`
	RecoverFreePct   float64 `json:"recover_free_pct"`
	// HeadroomBytes is required temp/build expansion room, added on top of
	// MinFreeBytes when deciding capacity (block floor = min + headroom).
	HeadroomBytes uint64 `json:"headroom_bytes,omitempty"`
}

// maxDiskFloorBytes saturates threshold conversion (2^62) so absurd or
// hostile env values cannot overflow uint64 arithmetic and silently
// produce a tiny floor — an overflow must fail closed, not open.
const maxDiskFloorBytes = uint64(1) << 62

// gbToBytes converts with saturation at maxDiskFloorBytes.
func gbToBytes(gb float64) uint64 {
	b := gb * bytesPerGiB
	if b >= float64(maxDiskFloorBytes) {
		return maxDiskFloorBytes
	}
	return uint64(b)
}

// satAdd adds with saturation — floor arithmetic must never wrap toward a
// tiny (fail-open) value.
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// satMul2 doubles with saturation.
func satMul2(a uint64) uint64 {
	if a > math.MaxUint64/2 {
		return math.MaxUint64
	}
	return 2 * a
}

// blockFreeBytes is the effective byte floor: reserve plus required
// temp/build headroom, saturating.
func (t DiskThresholds) blockFreeBytes() uint64 {
	return satAdd(t.MinFreeBytes, t.HeadroomBytes)
}

// DiskPressureError is the structured BLOCKED evidence emitted when a fleet
// mutation is refused. State is always BLOCKED while the error is returned;
// Reason distinguishes fresh pressure, an unreadable stat, and the
// hysteresis window (recovering: above block floor but below recover floor).
type DiskPressureError struct {
	State      string         `json:"state"` // BLOCKED
	Reason     string         `json:"reason"`
	Operation  string         `json:"operation"`
	Stats      []DiskStat     `json:"stats,omitempty"`
	Thresholds DiskThresholds `json:"thresholds"`
	Detail     string         `json:"detail,omitempty"`
	NextAction string         `json:"next_action"`
	// OutstandingReservedBytes is admitted-but-unreleased fan-out capacity
	// already subtracted from the Stats free-space figures.
	OutstandingReservedBytes uint64 `json:"outstanding_reserved_bytes,omitempty"`
}

const (
	ReasonDiskPressure   = "disk_pressure"
	ReasonStatUnreadable = "disk_stat_unreadable"
	ReasonRecovering     = "disk_pressure_recovering"
	ReasonScopeUnknown   = "disk_scope_unknown"
)

const safeNextAction = "refusing new fleet mutations; nothing was deleted — reclaim space only via the safe-GC contract (never ad-hoc force removal), then retry after a fresh probe shows headroom above the recover threshold"

func (e *DiskPressureError) Error() string {
	evidence, _ := json.Marshal(e)
	return fmt.Sprintf("disk pressure guard: %s %s for operation %q: %s; %s; evidence: %s",
		e.State, e.Reason, e.Operation, e.Detail, e.NextAction, evidence)
}

// DiskProber probes the volume containing path. Injectable for tests.
type DiskProber func(path string) (DiskStat, error)

// DiskGuardState is the control-plane projection of the guard, mirroring
// the FAC-150 provider-lane pattern (ok | blocked | recovering).
type DiskGuardState string

const (
	// DiskOK — headroom above every enabled floor; mutations proceed.
	DiskOK DiskGuardState = "ok"
	// DiskBlocked — pressure or unreadable stat; all fleet mutations refused.
	DiskBlocked DiskGuardState = "blocked"
	// DiskRecovering — above the block floor but below the recover floor;
	// still refusing until a fresh probe shows stable headroom (hysteresis).
	DiskRecovering DiskGuardState = "recovering"
)

// DiskGuard holds hysteresis state: once blocked, it stays blocked until a
// fresh probe clears the (higher) recover thresholds. State is in-process
// only — after a restart the first Check reconciles state from a fresh
// probe (there is nothing stale to persist or replay).
type DiskGuard struct {
	// opMu serializes probe + state transition as one unit: without it,
	// concurrent checks could apply stale observations out of order and
	// parallel callers could all admit against the same free-space
	// snapshot.
	opMu sync.Mutex

	mu           sync.Mutex
	prober       DiskProber
	state        DiskGuardState
	lastEvidence *DiskPressureError
	// outstanding is the sum of admitted-but-unreleased capacity
	// reservations, subtracted from observed free space so concurrent
	// fan-out is bounded by real remaining headroom.
	outstanding uint64
	sink        DiskEvidenceSink
}

// DiskEvidenceSink receives structured pressure state TRANSITIONS for
// durable projection (provider/Kaneo lifecycle path). The sink only
// observes; fencing and durable write semantics stay with the provider
// projection owner (FAC-147) — this is the seam, not a duplicate.
type DiskEvidenceSink interface {
	RecordDiskState(state DiskGuardState, evidence *DiskPressureError)
}

// SetEvidenceSink installs the projection sink. Pass nil to detach.
func (g *DiskGuard) SetEvidenceSink(s DiskEvidenceSink) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sink = s
}

// NewDiskGuard returns a guard using prober, or the real statfs prober when
// prober is nil.
func NewDiskGuard(prober DiskProber) *DiskGuard {
	if prober == nil {
		prober = realDiskStat
	}
	return &DiskGuard{prober: prober}
}

// DefaultDiskGuard is the process-wide guard consulted by fleet mutation
// call sites via CheckDiskPressure.
var DefaultDiskGuard = NewDiskGuard(nil)

// CheckDiskPressure fails closed when any volume containing the given paths
// is under critical disk pressure, in the recovery hysteresis window, or has
// an unreadable disk stat. Call before any fleet mutation.
func CheckDiskPressure(operation string, paths ...string) error {
	return DefaultDiskGuard.Check(operation, paths...)
}

// Blocked reports whether the guard currently refuses fleet mutations
// (blocked or still inside the recovery hysteresis window).
func (g *DiskGuard) Blocked() bool {
	s := g.State()
	return s == DiskBlocked || s == DiskRecovering
}

// State returns the projection state. A fresh guard (no probe yet) reports
// DiskOK; the first Check reconciles from a live probe.
func (g *DiskGuard) State() DiskGuardState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == "" {
		return DiskOK
	}
	return g.state
}

// Status is the fleet/operator label, aligned with the FAC-150 provider
// style: ok | recovering | BLOCKED(disk_pressure) |
// BLOCKED(disk_stat_unreadable). Pressure is never mapped to zero work or
// success — a blocked guard is always visibly BLOCKED.
func (g *DiskGuard) Status() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case DiskBlocked:
		if g.lastEvidence != nil && g.lastEvidence.Reason == ReasonStatUnreadable {
			return "BLOCKED(disk_stat_unreadable)"
		}
		return "BLOCKED(disk_pressure)"
	case DiskRecovering:
		return "recovering"
	default:
		return "ok"
	}
}

// LastEvidence returns the structured evidence from the most recent refusal,
// or nil when the guard is ok. The pointer is the same struct returned as
// the Check error; treat it as read-only.
func (g *DiskGuard) LastEvidence() *DiskPressureError {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastEvidence
}

// probeAll probes every distinct volume under paths (deduped by fs
// identity). An unreadable stat returns fail-closed BLOCKED evidence.
func (g *DiskGuard) probeAll(operation string, th DiskThresholds, paths []string) ([]DiskStat, *DiskPressureError) {
	var stats []DiskStat
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		st, err := g.prober(p)
		if err != nil {
			return nil, &DiskPressureError{
				State:      "BLOCKED",
				Reason:     ReasonStatUnreadable,
				Operation:  operation,
				Thresholds: th,
				Detail:     fmt.Sprintf("cannot stat a scoped volume (failing closed): %v", redactErr(err)),
				NextAction: safeNextAction,
			}
		}
		if seen[st.FSID] {
			continue
		}
		seen[st.FSID] = true
		st.Volume = "vol-" + st.FSID
		stats = append(stats, st)
	}
	return stats, nil
}

// Check probes every distinct volume under paths and fails closed on
// pressure, recovery-in-progress, or an unreadable stat.
func (g *DiskGuard) Check(operation string, paths ...string) error {
	g.opMu.Lock()
	defer g.opMu.Unlock()
	th := loadDiskThresholds()
	stats, unreadable := g.probeAll(operation, th, paths)
	return g.evaluate(operation, th, stats, unreadable)
}

// Admit is THE common admission/reservation gate for disk-growing actions
// (worktree create, dispatch/launch, review/approve/renudge side effects,
// archive/integration, future verifier fan-out). Probe + state transition
// run serialized; on admission the configured per-mutation headroom is
// reserved and subtracted from every concurrent caller's view until the
// returned release func runs. Release is idempotent.
func (g *DiskGuard) Admit(operation string, paths ...string) (func(), error) {
	g.opMu.Lock()
	defer g.opMu.Unlock()
	th := loadDiskThresholds()
	stats, unreadable := g.probeAll(operation, th, paths)
	if err := g.evaluate(operation, th, stats, unreadable); err != nil {
		return nil, err
	}
	res := th.HeadroomBytes
	g.mu.Lock()
	g.outstanding += res
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.outstanding >= res {
				g.outstanding -= res
			} else {
				g.outstanding = 0
			}
			g.mu.Unlock()
		})
	}, nil
}

// AdmitDiskMutation is Admit on the process-wide default guard.
func AdmitDiskMutation(operation string, paths ...string) (func(), error) {
	return DefaultDiskGuard.Admit(operation, paths...)
}

// evaluate drives the state machine from one probe result. Both Check and
// Advise route through here, so an unreadable probe ALWAYS blocks — unknown
// capacity is never permission for even one mutation.
func (g *DiskGuard) evaluate(operation string, th DiskThresholds, stats []DiskStat, unreadable *DiskPressureError) error {
	g.mu.Lock()
	prev := g.state
	err := g.evaluateLocked(operation, th, stats, unreadable)
	next, ev, sink := g.state, g.lastEvidence, g.sink
	g.mu.Unlock()
	// Fire the projection seam only on state TRANSITIONS (including the
	// first reconcile from ""), never on steady-state checks — bounded,
	// spam-free evidence for the provider/Kaneo lifecycle path.
	if sink != nil && prev != next {
		sink.RecordDiskState(next, ev)
	}
	return err
}

// adjustForOutstanding subtracts admitted-but-unreleased reservations from
// observed free space so concurrent fan-out is bounded by real remaining
// headroom, not N copies of the same snapshot.
func adjustForOutstanding(stats []DiskStat, out uint64) []DiskStat {
	if out == 0 {
		return stats
	}
	adj := make([]DiskStat, len(stats))
	copy(adj, stats)
	for i := range adj {
		if adj[i].FreeBytes > out {
			adj[i].FreeBytes -= out
		} else {
			adj[i].FreeBytes = 0
		}
		if adj[i].TotalBytes > 0 {
			adj[i].FreePct = float64(adj[i].FreeBytes) / float64(adj[i].TotalBytes) * 100
		}
	}
	return adj
}

// evaluateLocked drives the state machine; caller holds g.mu.
func (g *DiskGuard) evaluateLocked(operation string, th DiskThresholds, stats []DiskStat, unreadable *DiskPressureError) error {
	// Zero probed volumes is NOT health — an unknown volume scope fails
	// closed exactly like an unreadable stat.
	if unreadable == nil && len(stats) == 0 {
		unreadable = &DiskPressureError{
			State:      "BLOCKED",
			Reason:     ReasonScopeUnknown,
			Operation:  operation,
			Thresholds: th,
			Detail:     "no volumes probed (empty or blank path scope); unknown capacity is never permission to mutate",
			NextAction: safeNextAction,
		}
	}
	if unreadable != nil {
		g.state = DiskBlocked
		g.lastEvidence = unreadable
		return unreadable
	}

	stats = adjustForOutstanding(stats, g.outstanding)

	if bad := below(stats, th.blockFreeBytes(), th.MinFreePct, th.MinInodePct); bad != nil {
		g.state = DiskBlocked
		pe := &DiskPressureError{
			State:                    "BLOCKED",
			Reason:                   ReasonDiskPressure,
			Operation:                operation,
			Stats:                    stats,
			Thresholds:               th,
			OutstandingReservedBytes: g.outstanding,
			Detail: fmt.Sprintf("volume %s free %.1fGiB (%.1f%%, %d free inodes) below reserve (min %.1fGiB / %.1f%%)",
				bad.Volume, float64(bad.FreeBytes)/bytesPerGiB, bad.FreePct, bad.FreeInodes,
				float64(th.blockFreeBytes())/bytesPerGiB, th.MinFreePct),
			NextAction: safeNextAction,
		}
		g.lastEvidence = pe
		return pe
	}

	// The recover floor applies while blocked/recovering AND on a fresh
	// process's first probe (state ""): a restart cannot know whether the
	// previous process was blocked, so landing inside the recovery band
	// reconstructs recovering conservatively instead of erasing hysteresis.
	if g.state == DiskBlocked || g.state == DiskRecovering || g.state == "" {
		if bad := below(stats, th.RecoverFreeBytes, th.RecoverFreePct, th.MinInodePct); bad != nil {
			g.state = DiskRecovering
			pe := &DiskPressureError{
				State:                    "BLOCKED",
				Reason:                   ReasonRecovering,
				Operation:                operation,
				Stats:                    stats,
				Thresholds:               th,
				OutstandingReservedBytes: g.outstanding,
				Detail: fmt.Sprintf("volume %s above block floor but below recover floor (%.1fGiB / %.1f%%); holding closed until stable headroom",
					bad.Volume, float64(th.RecoverFreeBytes)/bytesPerGiB, th.RecoverFreePct),
				NextAction: safeNextAction,
			}
			g.lastEvidence = pe
			return pe
		}
	}
	g.state = DiskOK
	g.lastEvidence = nil
	return nil
}

// DiskAdvice is the graduated capacity decision: proceed at full
// parallelism, serialize mutation fan-out, or refuse (fail closed).
type DiskAdvice struct {
	// Verdict is "proceed", "serialize", or "refuse".
	Verdict string `json:"verdict"`
	// MaxParallel is the advised mutation fan-out: 0 = no cap (proceed),
	// 1 = fully serialized, and refuse means no new work at all.
	MaxParallel int        `json:"max_parallel"`
	Detail      string     `json:"detail,omitempty"`
	Stats       []DiskStat `json:"stats,omitempty"`
	// Evidence is the structured refusal; non-nil only when Verdict is
	// "refuse" (it is the same error Check would return).
	Evidence *DiskPressureError `json:"evidence,omitempty"`
}

const (
	AdviceProceed   = "proceed"
	AdviceSerialize = "serialize"
	AdviceRefuse    = "refuse"
)

// Advise is the graduated form of Check: serialize or reduce mutation
// parallelism BEFORE rejecting all work. Below the block floor (or on an
// unreadable stat) it refuses exactly like Check, driving the same state
// machine; in the soft band (below 2x the effective floor by default,
// configurable via HERD_DISK_SERIALIZE_FREE_GB/_PCT) it advises
// MaxParallel=1. Fan-out call sites (verifier/mutation testing) consume
// this; single-mutation call sites use Check directly.
func (g *DiskGuard) Advise(operation string, paths ...string) DiskAdvice {
	g.opMu.Lock()
	defer g.opMu.Unlock()
	th := loadDiskThresholds()
	// ONE probe feeds both the hard-floor state machine and the soft band:
	// no second probe exists whose failure could be softened. An unreadable
	// stat refuses via evaluate exactly like Check.
	stats, unreadable := g.probeAll(operation, th, paths)
	if err := g.evaluate(operation, th, stats, unreadable); err != nil {
		var pe *DiskPressureError
		errors.As(err, &pe)
		return DiskAdvice{Verdict: AdviceRefuse, MaxParallel: 0, Evidence: pe,
			Detail: err.Error()}
	}

	softBytes := gbToBytes(envFloat(EnvDiskSerializeFreeGB, 0))
	if softBytes == 0 {
		softBytes = satMul2(th.blockFreeBytes())
	}
	softPct := envFloat(EnvDiskSerializeFreePct, 2*th.MinFreePct)

	if bad := below(stats, softBytes, softPct, 0); bad != nil {
		return DiskAdvice{
			Verdict:     AdviceSerialize,
			MaxParallel: 1,
			Stats:       stats,
			Detail: fmt.Sprintf("volume %s free %.1fGiB (%.1f%%) inside soft band (< %.1fGiB / %.1f%%): serializing mutation fan-out before refusing work",
				bad.Volume, float64(bad.FreeBytes)/bytesPerGiB, bad.FreePct,
				float64(softBytes)/bytesPerGiB, softPct),
		}
	}
	return DiskAdvice{Verdict: AdviceProceed, Stats: stats}
}

// AdviseDiskPressure is Advise on the process-wide default guard.
func AdviseDiskPressure(operation string, paths ...string) DiskAdvice {
	return DefaultDiskGuard.Advise(operation, paths...)
}

// redactErr strips path context from probe errors so persisted evidence
// never carries absolute host paths (raw paths stay process-local).
func redactErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// below returns the first stat under any enabled floor (zero disables an
// axis). Inode floor only applies when the filesystem reports inodes.
func below(stats []DiskStat, minFreeBytes uint64, minFreePct, minInodePct float64) *DiskStat {
	for i := range stats {
		s := &stats[i]
		if s.FreeBytes < minFreeBytes {
			return s
		}
		if s.FreePct < minFreePct {
			return s
		}
		if s.TotalInodes > 0 && s.InodeFreePct < minInodePct {
			return s
		}
	}
	return nil
}

func loadDiskThresholds() DiskThresholds {
	minGB := envFloat(EnvDiskMinFreeGB, defaultDiskMinFreeGB)
	minPct := envFloat(EnvDiskMinFreePct, defaultDiskMinFreePct)
	minInode := envFloat(EnvDiskMinInodePct, defaultDiskMinInodePct)
	defHead := defaultDiskBuildHeadroomGB
	if minGB == 0 && minPct == 0 && minInode == 0 {
		// Guard explicitly disabled on every axis: the derived headroom
		// default is off too. An explicit headroom env is always honored.
		defHead = 0
	}
	headGB := envFloat(EnvDiskBuildHeadroomGB, defHead)
	th := DiskThresholds{
		MinFreeBytes: gbToBytes(minGB),
		MinFreePct:   minPct,
		MinInodePct:  minInode,
		// Recover floor defaults scale from the EFFECTIVE block floor
		// (reserve + headroom) so hysteresis still clears above headroom.
		RecoverFreeBytes: gbToBytes(envFloat(EnvDiskRecoverFreeGB, (minGB+headGB)*recoverFactor)),
		RecoverFreePct:   envFloat(EnvDiskRecoverFreePct, minPct*recoverFactor),
		HeadroomBytes:    gbToBytes(headGB),
	}
	return th
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	// NaN/±Inf parse successfully and NaN < 0 is false — a NaN floor makes
	// every "below" comparison false and silently disables the gate. All
	// non-finite values fail closed to the protective default.
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return def
	}
	return n
}

// ProbeDisk exposes the real statfs probe for read-only observation
// surfaces (metrics, status). It never mutates guard state.
func ProbeDisk(path string) (DiskStat, error) {
	return realDiskStat(path)
}

// realDiskStat probes the volume containing path via statfs. If path does
// not exist yet (e.g. a worktree pool about to be created), it walks up to
// the nearest existing ancestor so a first-ever run is probed, not refused.
// Any other stat failure is returned and the caller fails closed.
func realDiskStat(path string) (DiskStat, error) {
	p := path
	var fi os.FileInfo
	for {
		info, err := os.Stat(p)
		if err == nil {
			fi = info
			break
		} else if !os.IsNotExist(err) {
			return DiskStat{}, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return DiskStat{}, fmt.Errorf("no existing ancestor for %q", path)
		}
		p = parent
	}

	// Filesystem identity via st_dev (portable across darwin/linux, unlike
	// the platform-divergent Statfs_t.Fsid field names).
	fsid := "fsid-unknown"
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		fsid = fmt.Sprintf("dev-%x", uint64(sys.Dev))
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return DiskStat{}, err
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bavail * bsize
	if total == 0 {
		return DiskStat{}, fmt.Errorf("volume at %q reports zero total blocks", p)
	}
	ds := DiskStat{
		Path:        p,
		FSID:        fsid,
		TotalBytes:  total,
		FreeBytes:   free,
		FreePct:     float64(free) / float64(total) * 100,
		TotalInodes: st.Files,
		FreeInodes:  st.Ffree,
	}
	if ds.TotalInodes > 0 {
		ds.InodeFreePct = float64(ds.FreeInodes) / float64(ds.TotalInodes) * 100
	}
	return ds, nil
}
