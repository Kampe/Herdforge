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
	"fmt"
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
	// (git object writes, race binaries, archives). Default 0 (opt-in) so
	// held packages' test shims stay valid; operators set it fleet-wide.
	EnvDiskBuildHeadroomGB = "HERD_DISK_BUILD_HEADROOM_GB"

	defaultDiskMinFreeGB   = 15.0
	defaultDiskMinFreePct  = 2.0
	defaultDiskMinInodePct = 1.0
	// Hysteresis: once blocked, recover only after a fresh probe shows
	// headroom above recoverFactor x the block threshold.
	recoverFactor = 1.25

	bytesPerGiB = float64(1 << 30)
)

// DiskStat is one volume's observed capacity headroom.
type DiskStat struct {
	Path         string  `json:"path"`
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

// blockFreeBytes is the effective byte floor: reserve plus required
// temp/build headroom.
func (t DiskThresholds) blockFreeBytes() uint64 {
	return t.MinFreeBytes + t.HeadroomBytes
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
}

const (
	ReasonDiskPressure   = "disk_pressure"
	ReasonStatUnreadable = "disk_stat_unreadable"
	ReasonRecovering     = "disk_pressure_recovering"
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
	mu           sync.Mutex
	prober       DiskProber
	state        DiskGuardState
	lastEvidence *DiskPressureError
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

// Check probes every distinct volume under paths and fails closed on
// pressure, recovery-in-progress, or an unreadable stat.
func (g *DiskGuard) Check(operation string, paths ...string) error {
	th := loadDiskThresholds()

	var stats []DiskStat
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		st, err := g.prober(p)
		if err != nil {
			pe := &DiskPressureError{
				State:      "BLOCKED",
				Reason:     ReasonStatUnreadable,
				Operation:  operation,
				Thresholds: th,
				Detail:     fmt.Sprintf("cannot stat volume for %q (failing closed): %v", p, err),
				NextAction: safeNextAction,
			}
			g.mu.Lock()
			g.state = DiskBlocked
			g.lastEvidence = pe
			g.mu.Unlock()
			return pe
		}
		if seen[st.FSID] {
			continue
		}
		seen[st.FSID] = true
		stats = append(stats, st)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if bad := below(stats, th.blockFreeBytes(), th.MinFreePct, th.MinInodePct); bad != nil {
		g.state = DiskBlocked
		pe := &DiskPressureError{
			State:      "BLOCKED",
			Reason:     ReasonDiskPressure,
			Operation:  operation,
			Stats:      stats,
			Thresholds: th,
			Detail: fmt.Sprintf("volume %s (%s) free %.1fGiB (%.1f%%, %d free inodes) below reserve (min %.1fGiB / %.1f%%)",
				bad.Path, bad.FSID, float64(bad.FreeBytes)/bytesPerGiB, bad.FreePct, bad.FreeInodes,
				float64(th.blockFreeBytes())/bytesPerGiB, th.MinFreePct),
			NextAction: safeNextAction,
		}
		g.lastEvidence = pe
		return pe
	}

	if g.state == DiskBlocked || g.state == DiskRecovering {
		if bad := below(stats, th.RecoverFreeBytes, th.RecoverFreePct, th.MinInodePct); bad != nil {
			g.state = DiskRecovering
			pe := &DiskPressureError{
				State:      "BLOCKED",
				Reason:     ReasonRecovering,
				Operation:  operation,
				Stats:      stats,
				Thresholds: th,
				Detail: fmt.Sprintf("volume %s above block floor but below recover floor (%.1fGiB / %.1f%%); holding closed until stable headroom",
					bad.Path, float64(th.RecoverFreeBytes)/bytesPerGiB, th.RecoverFreePct),
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
	headGB := envFloat(EnvDiskBuildHeadroomGB, 0)
	th := DiskThresholds{
		MinFreeBytes: uint64(minGB * bytesPerGiB),
		MinFreePct:   minPct,
		MinInodePct:  envFloat(EnvDiskMinInodePct, defaultDiskMinInodePct),
		// Recover floor defaults scale from the EFFECTIVE block floor
		// (reserve + headroom) so hysteresis still clears above headroom.
		RecoverFreeBytes: uint64(envFloat(EnvDiskRecoverFreeGB, (minGB+headGB)*recoverFactor) * bytesPerGiB),
		RecoverFreePct:   envFloat(EnvDiskRecoverFreePct, minPct*recoverFactor),
		HeadroomBytes:    uint64(headGB * bytesPerGiB),
	}
	return th
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
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
