package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Capacity is a read-only filesystem capacity observation.
type Capacity struct {
	FilesystemID            string
	TotalBytes, FreeBytes   uint64
	TotalInodes, FreeInodes uint64
}

// StatFSBackend is injectable so tests never probe or stress a live host.
type StatFSBackend interface {
	StatFS(path string) (Capacity, error)
}
type StatFSFunc func(string) (Capacity, error)

func (f StatFSFunc) StatFS(path string) (Capacity, error) { return f(path) }

// Policy contains deny thresholds and optional higher recovery thresholds.
type DiskPolicy struct {
	ReserveBytes, ReserveInodes   uint64
	ReservePercent                float64
	RecoveryBytes, RecoveryInodes uint64
	RecoveryPercent               float64
}

type DiskRequest struct {
	Operation, Path, TempPath     string
	RequiredBytes, RequiredInodes uint64
	PreviouslyBlocked             bool
}

type DiskState string

const (
	DiskReady   DiskState = "READY"
	DiskBlocked DiskState = "BLOCKED"
)

const (
	DiskReasonNone                 = "none"
	DiskReasonBelowThreshold       = "below_threshold"
	DiskReasonHysteresis           = "hysteresis"
	DiskReasonInodeExhaustion      = "inode_exhaustion"
	DiskReasonTempVolumeDivergence = "temp_volume_divergence"
	DiskReasonUnavailable          = "unavailable"
	DiskReasonInvalid              = "invalid"
	DiskReasonInvalidPolicy        = "invalid_policy"
)

// DiskEvidence is bounded and safe to serialize or log. Paths are never
// included; identities containing path-like data are reduced to an opaque ID.
type DiskEvidence struct {
	Kind             string  `json:"kind"`
	Reason           string  `json:"reason"`
	Operation        string  `json:"operation"`
	FilesystemID     string  `json:"filesystem_id,omitempty"`
	TempFilesystemID string  `json:"temp_filesystem_id,omitempty"`
	FreeBytes        uint64  `json:"free_bytes"`
	FreePercent      float64 `json:"free_percent"`
	FreeInodes       uint64  `json:"free_inodes"`
	RequiredBytes    uint64  `json:"required_bytes"`
	ReserveBytes     uint64  `json:"reserve_bytes"`
	ReservePercent   float64 `json:"reserve_percent"`
	ReserveInodes    uint64  `json:"reserve_inodes"`
	TempFreeBytes    uint64  `json:"temp_free_bytes,omitempty"`
	TempFreePercent  float64 `json:"temp_free_percent,omitempty"`
	TempFreeInodes   uint64  `json:"temp_free_inodes,omitempty"`
}

type DiskDecision struct {
	State    DiskState    `json:"state"`
	Allowed  bool         `json:"allowed"`
	Evidence DiskEvidence `json:"evidence"`
}

var diskOperationPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,31}$`)

// Evaluate is a pure fail-closed gate. It only probes through backend and
// performs no claim, worktree, process, archive, delete, or cleanup action.
func EvaluateDiskCapacity(backend StatFSBackend, request DiskRequest, policy DiskPolicy) DiskDecision {
	t := policy.thresholds(request.PreviouslyBlocked)
	e := DiskEvidence{Kind: "disk_pressure", Operation: boundedDiskOperation(request.Operation), ReserveBytes: t.bytes, ReservePercent: t.percent, ReserveInodes: t.inodes, RequiredBytes: request.RequiredBytes}
	if err := validDiskPolicy(policy); err != nil {
		return diskBlocked(e, DiskReasonInvalidPolicy)
	}
	if backend == nil {
		return diskBlocked(e, DiskReasonUnavailable)
	}
	root, err := backend.StatFS(request.Path)
	if err != nil {
		return diskBlocked(e, DiskReasonUnavailable)
	}
	if err := validCapacity(root); err != nil {
		return diskBlocked(e, DiskReasonInvalid)
	}
	e.FilesystemID = safeDiskIdentity(root.FilesystemID)
	e.FreeBytes, e.FreePercent, e.FreeInodes = capacityMetrics(root)
	if !capacityMeets(root, request.RequiredBytes, request.RequiredInodes, t) {
		reason := DiskReasonBelowThreshold
		if request.PreviouslyBlocked {
			reason = DiskReasonHysteresis
		}
		if capacityShortOnInodes(root.FreeInodes, request.RequiredInodes, t.inodes) {
			reason = DiskReasonInodeExhaustion
		}
		return diskBlocked(e, reason)
	}
	if request.TempPath != "" {
		tmp, err := backend.StatFS(request.TempPath)
		if err != nil {
			return diskBlocked(e, DiskReasonUnavailable)
		}
		if err := validCapacity(tmp); err != nil {
			return diskBlocked(e, DiskReasonInvalid)
		}
		e.TempFilesystemID = safeDiskIdentity(tmp.FilesystemID)
		e.TempFreeBytes, e.TempFreePercent, e.TempFreeInodes = capacityMetrics(tmp)
		if !capacityMeets(tmp, request.RequiredBytes, request.RequiredInodes, t) {
			return diskBlocked(e, DiskReasonTempVolumeDivergence)
		}
	}
	e.Reason = DiskReasonNone
	return DiskDecision{State: DiskReady, Allowed: true, Evidence: e}
}

type diskThresholds struct {
	bytes, inodes uint64
	percent       float64
}

func (p DiskPolicy) thresholds(recovery bool) diskThresholds {
	t := diskThresholds{p.ReserveBytes, p.ReserveInodes, p.ReservePercent}
	if recovery {
		if p.RecoveryBytes > t.bytes {
			t.bytes = p.RecoveryBytes
		}
		if p.RecoveryInodes > t.inodes {
			t.inodes = p.RecoveryInodes
		}
		if p.RecoveryPercent > t.percent {
			t.percent = p.RecoveryPercent
		}
	}
	return t
}
func validCapacity(c Capacity) error {
	if c.TotalBytes == 0 || c.TotalInodes == 0 || c.FreeBytes > c.TotalBytes || c.FreeInodes > c.TotalInodes {
		return fmt.Errorf("invalid capacity")
	}
	return nil
}
func validDiskPolicy(p DiskPolicy) error {
	for _, value := range []float64{p.ReservePercent, p.RecoveryPercent} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return fmt.Errorf("invalid reserve percentage")
		}
	}
	return nil
}
func capacityFromStatfs(filesystemID, filesystemType string, blocks, available, blockSize, files, freeFiles uint64) (Capacity, error) {
	if blockSize == 0 || blocks > math.MaxUint64/blockSize || available > math.MaxUint64/blockSize {
		return Capacity{}, fmt.Errorf("statfs byte counter overflow")
	}
	return Capacity{FilesystemID: filesystemIdentity(filesystemID, filesystemType), TotalBytes: blocks * blockSize, FreeBytes: available * blockSize, TotalInodes: files, FreeInodes: freeFiles}, nil
}
func filesystemIdentity(fsid, filesystemType string) string {
	h := sha256.Sum256([]byte(filesystemType + ":" + fsid))
	return "opaque:" + hex.EncodeToString(h[:6])
}
func capacityMetrics(c Capacity) (uint64, float64, uint64) {
	return c.FreeBytes, float64(c.FreeBytes) * 100 / float64(c.TotalBytes), c.FreeInodes
}
func capacityMeets(c Capacity, requiredBytes, requiredInodes uint64, t diskThresholds) bool {
	if requiredBytes > math.MaxUint64-t.bytes || c.FreeBytes < requiredBytes+t.bytes {
		return false
	}
	if requiredInodes > math.MaxUint64-t.inodes || c.FreeInodes < requiredInodes+t.inodes {
		return false
	}
	return float64(c.FreeBytes)*100/float64(c.TotalBytes) >= t.percent
}

func capacityShortOnInodes(free, required, reserve uint64) bool {
	return required > math.MaxUint64-reserve || free < required+reserve
}
func diskBlocked(e DiskEvidence, reason string) DiskDecision {
	e.Reason = reason
	return DiskDecision{State: DiskBlocked, Evidence: e}
}
func boundedDiskOperation(s string) string {
	s = strings.TrimSpace(s)
	if !diskOperationPattern.MatchString(s) {
		return "unknown"
	}
	return s
}
func safeDiskIdentity(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) <= 64 && !strings.ContainsAny(s, `/\\`) {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return "opaque:" + hex.EncodeToString(h[:6])
}
