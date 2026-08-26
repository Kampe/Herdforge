package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

// FAC-635: PlanDiskAdmission summed the requirement of EVERY request in a batch,
// so a harvest over 333 candidate worktrees demanded 333 x 192 MiB = 62 GiB up
// front. The harvest fan-out is bounded by a semaphore at min(max(NumCPU,2),8),
// so at most EIGHT run at once and the real peak is 1.5 GiB. The gate refused
// work on a disk with 77 GB free, and it tightened as the backlog grew: the more
// work waiting, the more certainly harvest was blocked. Past roughly 420
// worktrees it would have exceeded the whole volume and harvest could never have
// run again regardless of free space.
//
// Sum-of-all-work is the wrong measure for a bounded pipeline; peak-concurrent is
// the right one.

// DiskRequirement describes bounded headroom for one disk-growing operation.
type DiskRequirement struct {
	Bytes  uint64
	Inodes uint64
}

const (
	worktreeCreateBytes  = 64 * (1 << 20)
	worktreeCreateInodes = 128
	mergeBytes           = 128 * (1 << 20)
	mergeInodes          = 256
)

func DefaultWorktreeCreateRequirement() DiskRequirement {
	return DiskRequirement{Bytes: worktreeCreateBytes, Inodes: worktreeCreateInodes}
}

func DefaultMergeRequirement() DiskRequirement {
	return DiskRequirement{Bytes: mergeBytes, Inodes: mergeInodes}
}

// AggregateDiskRequirement adds independent artifact/build headroom without
// wrapping. Overflow is a policy/configuration error and must fail closed.
func AggregateDiskRequirement(parts ...DiskRequirement) (DiskRequirement, error) {
	var total DiskRequirement
	for _, part := range parts {
		if part.Bytes > math.MaxUint64-total.Bytes || part.Inodes > math.MaxUint64-total.Inodes {
			return DiskRequirement{}, fmt.Errorf("disk requirement overflow")
		}
		total.Bytes += part.Bytes
		total.Inodes += part.Inodes
	}
	if total.Bytes == 0 || total.Inodes == 0 {
		return DiskRequirement{}, fmt.Errorf("disk requirement must be nonzero")
	}
	return total, nil
}

// DefaultDiskPolicy is intentionally conservative for disk-growing fleet
// operations. Callers may supply a policy explicitly for hermetic tests or
// deployment-specific capacity reservations.
func DefaultDiskPolicy() DiskPolicy {
	return DiskPolicy{
		ReserveBytes:    15 * (1 << 30),
		ReservePercent:  2,
		ReserveInodes:   1,
		RecoveryBytes:   19 * (1 << 30),
		RecoveryPercent: 2.5,
		RecoveryInodes:  1,
	}
}

// ResolveExistingPath resolves a path for a read-only volume probe without
// creating the destination. This is required for a first worktree creation,
// where the pool directory does not exist yet.
func ResolveExistingPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("probe path is empty")
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(p); err == nil {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return "", err
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(p)
		if next == p {
			return "", fmt.Errorf("no existing ancestor for probe path")
		}
		p = next
	}
}

type DiskRequest struct {
	Operation, Path, TempPath     string
	RequiredBytes, RequiredInodes uint64
	PreviouslyBlocked             bool
	AdditionalPaths               []string
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
	// DiskReasonInvalidRequest separates a malformed CALLER REQUEST from a real
	// capacity problem. Both used to report reason="invalid" with a zeroed
	// DiskEvidence, so a harvest request that simply named no byte/inode
	// requirement surfaced as
	//   state=BLOCKED {"kind":"disk_pressure","reason":"invalid","free_bytes":0,...}
	// which reads as a full disk. A lane hit exactly this and went to check df,
	// which showed 114 GiB free. The gate was right to refuse and wrong about why.
	DiskReasonInvalidRequest        = "invalid_request"
	DiskReasonInvalidPolicy         = "invalid_policy"
	DiskReasonAdditionalUnavailable = "additional_volume_unavailable"
	DiskReasonAdditionalInvalid     = "additional_volume_invalid"
	DiskReasonAdditionalBelow       = "additional_volume_below_threshold"
)

const (
	DiskActionProceed      = "proceed"
	DiskActionRetryProbe   = "retry_capacity_probe"
	DiskActionFixPolicy    = "fix_disk_policy"
	DiskActionRecoverSpace = "recover_capacity_without_cleanup"
)

// DiskEvidence is bounded and safe to serialize or log. Paths are never
// included; identities containing path-like data are reduced to an opaque ID.
type DiskEvidence struct {
	Kind               string   `json:"kind"`
	Reason             string   `json:"reason"`
	Operation          string   `json:"operation"`
	FilesystemID       string   `json:"filesystem_id,omitempty"`
	TempFilesystemID   string   `json:"temp_filesystem_id,omitempty"`
	FreeBytes          uint64   `json:"free_bytes"`
	FreePercent        float64  `json:"free_percent"`
	FreeInodes         uint64   `json:"free_inodes"`
	RequiredBytes      uint64   `json:"required_bytes"`
	ReserveBytes       uint64   `json:"reserve_bytes"`
	ReservePercent     float64  `json:"reserve_percent"`
	ReserveInodes      uint64   `json:"reserve_inodes"`
	RequiredInodes     uint64   `json:"required_inodes"`
	TempFreeBytes      *uint64  `json:"temp_free_bytes,omitempty"`
	TempFreePercent    *float64 `json:"temp_free_percent,omitempty"`
	TempFreeInodes     *uint64  `json:"temp_free_inodes,omitempty"`
	ScopeID            string   `json:"scope_id,omitempty"`
	FailedFilesystemID string   `json:"failed_filesystem_id,omitempty"`
	FailedFreeBytes    *uint64  `json:"failed_free_bytes,omitempty"`
	FailedFreePercent  *float64 `json:"failed_free_percent,omitempty"`
	FailedFreeInodes   *uint64  `json:"failed_free_inodes,omitempty"`
	NextAction         string   `json:"next_action"`
	// Reclaimable names where space can be recovered WITHOUT touching fleet
	// state, largest first (FAC-654). A gate that reports only "below
	// threshold" tells an operator that they are stuck; it does not tell them
	// that gigabytes of rebuildable cache are sitting next to the reserve.
	Reclaimable []ReclaimableClass `json:"reclaimable,omitempty"`
}

// ReclaimableClass is one bucket of space that can be recovered without
// deleting any unique work: build caches, download caches, dead cache
// generations. Never worktrees, branches, leases, or ledgers -- those are
// fleet state and are the operator's call, not a gate's suggestion.
type ReclaimableClass struct {
	Path    string `json:"path"`
	Bytes   uint64 `json:"bytes"`
	Kind    string `json:"kind"`
	Rebuild string `json:"rebuild"`
}

type DiskDecision struct {
	State    DiskState    `json:"state"`
	Allowed  bool         `json:"allowed"`
	Evidence DiskEvidence `json:"evidence"`
}

// DiskAdmission is the read-only, fail-closed boundary used by mutation
// owners. Implementations must not create, remove, or otherwise alter paths.
type DiskAdmission interface {
	Admit(DiskRequest) DiskDecision
}

type DiskAdmissionFunc func(DiskRequest) DiskDecision

func (f DiskAdmissionFunc) Admit(request DiskRequest) DiskDecision { return f(request) }

// DiskAdmissionPlan is the complete set of capacity scopes for one
// concurrent mutation batch. Requests are sorted by opaque filesystem ID so
// admission order is deterministic and every scope is admitted exactly once.
type DiskAdmissionPlan struct {
	Requests []DiskRequest
	Scopes   []string
}

// DiskPlanProvider lets an admission authority resolve canonical volume
// identities and aggregate a complete batch before any callback is launched.
type DiskPlanProvider interface {
	PlanDiskAdmission([]DiskRequest) (DiskAdmissionPlan, error)
}

// BatchDiskAdmission admits an already-authoritative plan. Implementations
// must not probe or mutate between scopes and must return the first denial.
type BatchDiskAdmission interface {
	AdmitDiskPlan(DiskAdmissionPlan) error
}

// DiskAdmissionError retains the bounded, path-free evidence produced by a
// denied scope. Callers must propagate this error rather than reconstructing
// a lossy reason string.
type DiskAdmissionError struct {
	Scope    string
	Decision DiskDecision
}

func (e *DiskAdmissionError) Error() string {
	evidence := marshalDiskEvidence(e.Decision.Evidence)
	// A malformed request is not a capacity problem, so it must not be announced
	// as the "disk capacity gate" blocking on zeroed capacity evidence. That
	// phrasing sent a lane to check df, which reported 114 GiB free, and the real
	// fault -- a harvest request naming no byte/inode requirement -- went unread.
	if e.Decision.Evidence.Reason == DiskReasonInvalidRequest {
		return fmt.Sprintf("harvest admission request is malformed (scope %s): state=%s evidence=%s; "+
			"the disk was never the problem: no byte/inode requirement was supplied, so capacity was never evaluated. "+
			"Fix the caller's DiskRequest; retrying the capacity probe cannot help",
			e.Scope, e.Decision.State, evidence)
	}
	message := fmt.Sprintf("disk capacity gate blocked scope %s: state=%s evidence=%s", e.Scope, e.Decision.State, evidence)
	if e.Decision.Evidence.NextAction == DiskActionRecoverSpace {
		message += "; next step: host-level intervention is required; contact your operator"
	}
	return message
}

func marshalDiskEvidence(evidence DiskEvidence) []byte {
	clean := func(value float64) float64 {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0
		}
		return value
	}
	evidence.FreePercent = clean(evidence.FreePercent)
	evidence.ReservePercent = clean(evidence.ReservePercent)
	if evidence.TempFreePercent != nil {
		value := clean(*evidence.TempFreePercent)
		evidence.TempFreePercent = &value
	}
	if evidence.FailedFreePercent != nil {
		value := clean(*evidence.FailedFreePercent)
		evidence.FailedFreePercent = &value
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return []byte(`{"kind":"disk_pressure","reason":"invalid","next_action":"retry_capacity_probe"}`)
	}
	return data
}

// AdmitDiskPlan admits every planned scope once and stops at the first
// denial; no mutation callback is reachable until this returns nil.
func AdmitDiskPlan(admission DiskAdmission, plan DiskAdmissionPlan) error {
	if admission == nil {
		return fmt.Errorf("disk capacity gate unavailable")
	}
	if len(plan.Requests) == 0 || len(plan.Requests) != len(plan.Scopes) {
		return fmt.Errorf("disk capacity gate: incomplete admission plan")
	}
	seen := make(map[string]struct{}, len(plan.Scopes))
	for _, scope := range plan.Scopes {
		if scope == "" {
			return fmt.Errorf("disk capacity gate: incomplete admission plan")
		}
		if _, ok := seen[scope]; ok {
			return fmt.Errorf("disk capacity gate: duplicate admission scope %s", scope)
		}
		seen[scope] = struct{}{}
	}
	if batch, ok := admission.(BatchDiskAdmission); ok {
		return batch.AdmitDiskPlan(plan)
	}
	return fmt.Errorf("disk capacity gate: batch admission unavailable")
}

// CapacityGate adds hysteresis to the pure evaluator while keeping probing
// and policy explicit. The mutex makes one gate safe for concurrent callers.
type CapacityGate struct {
	Backend StatFSBackend
	Policy  DiskPolicy

	mu      sync.Mutex
	blocked map[string]bool
}

func NewCapacityGate(backend StatFSBackend, policy DiskPolicy) *CapacityGate {
	return &CapacityGate{Backend: backend, Policy: policy, blocked: make(map[string]bool)}
}

func (g *CapacityGate) Admit(request DiskRequest) DiskDecision {
	if g == nil {
		return EvaluateDiskCapacity(nil, request, DiskPolicy{})
	}
	// Scope is derived here, never accepted from a caller or persisted input.
	// The inputs are the canonical paths actually sent to the probe backend.
	scope := CapacityScopeForPaths(append([]string{request.Path, request.TempPath}, request.AdditionalPaths...)...)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked == nil {
		g.blocked = make(map[string]bool)
	}
	request.PreviouslyBlocked = g.blocked[scope]
	decision := EvaluateDiskCapacity(g.Backend, request, g.Policy)
	decision.Evidence.ScopeID = scope
	if decision.Allowed {
		delete(g.blocked, scope)
	} else {
		g.blocked[scope] = true
	}
	return decision
}

// PlanDiskAdmission resolves every path once, groups requirements by the
// canonical filesystem identity reported by the read-only backend, and
// aggregates bytes/inodes with overflow checks. Each volume receives the
// total headroom for all operations that can grow it.
func (g *CapacityGate) PlanDiskAdmission(requests []DiskRequest) (DiskAdmissionPlan, error) {
	return g.PlanDiskAdmissionBounded(requests, 0)
}

// PlanDiskAdmissionBounded sizes the plan for at most maxConcurrent simultaneous
// operations per filesystem. maxConcurrent <= 0 is unbounded, preserving the old
// behaviour for callers that genuinely need every operation admitted at once.
func (g *CapacityGate) PlanDiskAdmissionBounded(requests []DiskRequest, maxConcurrent int) (DiskAdmissionPlan, error) {
	if g == nil || g.Backend == nil {
		return DiskAdmissionPlan{}, fmt.Errorf("disk capacity gate unavailable")
	}
	totals := make(map[string]DiskRequirement)
	paths := make(map[string]string)
	seenPerVolume := make(map[string]int)
	for _, request := range requests {
		if request.RequiredBytes == 0 || request.RequiredInodes == 0 {
			return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch", RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}, DiskReasonInvalidRequest)}
		}
		seen := make(map[string]struct{})
		seenVolumes := make(map[string]struct{})
		for _, path := range append([]string{request.Path, request.TempPath}, request.AdditionalPaths...) {
			if strings.TrimSpace(path) == "" {
				continue
			}
			canonical, err := ResolveExistingPath(path)
			if err != nil {
				return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch", RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}, DiskReasonUnavailable)}
			}
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			capacity, err := g.Backend.StatFS(canonical)
			if err != nil {
				return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch", RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}, DiskReasonUnavailable)}
			}
			if err := validCapacity(capacity); err != nil || strings.TrimSpace(capacity.FilesystemID) == "" {
				return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch", RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}, DiskReasonInvalid)}
			}
			id := safeDiskIdentity(capacity.FilesystemID)
			if _, ok := seenVolumes[id]; ok {
				continue
			}
			seenVolumes[id] = struct{}{}
			current := totals[id]
			combined, err := AggregateDiskRequirement(current, DiskRequirement{Bytes: request.RequiredBytes, Inodes: request.RequiredInodes})
			if err != nil {
				return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: id, Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch", FilesystemID: id, RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}, DiskReasonInvalid)}
			}
			// Cap the per-filesystem aggregate at the pipeline's real peak. Past
			// maxConcurrent items the total stops growing, because no more than
			// that many ever exist on disk at the same moment.
			if maxConcurrent > 0 {
				if seenPerVolume[id] >= maxConcurrent {
					continue
				}
				seenPerVolume[id]++
			}
			totals[id] = combined
			paths[id] = canonical
		}
	}
	ids := make([]string, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan := DiskAdmissionPlan{Requests: make([]DiskRequest, 0, len(ids)), Scopes: ids}
	for _, id := range ids {
		plan.Requests = append(plan.Requests, DiskRequest{Operation: "harvest_batch", Path: paths[id], RequiredBytes: totals[id].Bytes, RequiredInodes: totals[id].Inodes})
	}
	if len(plan.Requests) == 0 {
		return DiskAdmissionPlan{}, &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch"}, DiskReasonInvalidRequest)}
	}
	return plan, nil
}

// AdmitDiskPlan preserves hysteresis by canonical filesystem identity rather
// than by whichever path happened to represent that volume in the batch.
func (g *CapacityGate) AdmitDiskPlan(plan DiskAdmissionPlan) error {
	if g == nil {
		return fmt.Errorf("disk capacity gate unavailable")
	}
	if len(plan.Requests) != len(plan.Scopes) || len(plan.Requests) == 0 {
		return fmt.Errorf("disk capacity gate: incomplete admission plan")
	}
	seen := make(map[string]struct{}, len(plan.Scopes))
	for _, scope := range plan.Scopes {
		if scope == "" {
			return fmt.Errorf("disk capacity gate: incomplete admission plan")
		}
		if _, ok := seen[scope]; ok {
			return fmt.Errorf("disk capacity gate: duplicate admission scope %s", scope)
		}
		seen[scope] = struct{}{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	decisions := make([]DiskDecision, len(plan.Requests))
	failed := -1
	for i, request := range plan.Requests {
		request.PreviouslyBlocked = g.blocked[plan.Scopes[i]]
		decisions[i] = EvaluateDiskCapacity(g.Backend, request, g.Policy)
		if !decisions[i].Allowed {
			if failed < 0 {
				failed = i
			}
		}
	}
	if len(decisions) != len(plan.Requests) || len(decisions) != len(plan.Scopes) {
		return &DiskAdmissionError{Scope: "plan", Decision: diskBlocked(DiskEvidence{Operation: "harvest_batch"}, DiskReasonInvalid)}
	}
	for i, decision := range decisions {
		freshID := strings.TrimSpace(decision.Evidence.FilesystemID)
		if !validFilesystemIdentity(freshID) || safeDiskIdentity(freshID) != plan.Scopes[i] {
			return &DiskAdmissionError{Scope: plan.Scopes[i], Decision: DiskDecision{
				State: DiskBlocked,
				Evidence: DiskEvidence{
					Kind: "disk_pressure", Reason: DiskReasonInvalid,
					FilesystemID: safeDiskIdentity(freshID), ScopeID: plan.Scopes[i],
					NextAction: DiskActionRetryProbe,
				},
			}}
		}
		decisions[i].Evidence.ScopeID = plan.Scopes[i]
	}
	if g.blocked == nil {
		g.blocked = make(map[string]bool)
	}
	for i := range decisions {
		if decisions[i].Allowed {
			delete(g.blocked, plan.Scopes[i])
		} else {
			g.blocked[plan.Scopes[i]] = true
		}
	}
	if failed >= 0 {
		return &DiskAdmissionError{Scope: plan.Scopes[failed], Decision: decisions[failed]}
	}
	return nil
}

func validFilesystemIdentity(identity string) bool {
	return strings.TrimSpace(identity) != "" && safeDiskIdentity(identity) != "unknown"
}

// CapacityScopeForPaths returns a stable opaque identity for a canonical set
// of capacity inputs. It intentionally never returns or logs the paths.
func CapacityScopeForPaths(paths ...string) string {
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			values = append(values, filepath.Clean(path))
		}
	}
	sort.Strings(values)
	h := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return "scope:" + hex.EncodeToString(h[:8])
}

var diskOperationPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,31}$`)

// Evaluate is a pure fail-closed gate. It only probes through backend and
// performs no claim, worktree, process, archive, delete, or cleanup action.
func EvaluateDiskCapacity(backend StatFSBackend, request DiskRequest, policy DiskPolicy) DiskDecision {
	t := policy.thresholds(request.PreviouslyBlocked)
	e := DiskEvidence{Kind: "disk_pressure", Operation: boundedDiskOperation(request.Operation), ReserveBytes: t.bytes, ReservePercent: t.percent, ReserveInodes: t.inodes, RequiredBytes: request.RequiredBytes, RequiredInodes: request.RequiredInodes}
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
	if strings.TrimSpace(root.FilesystemID) == "" {
		return diskBlocked(e, DiskReasonUnavailable)
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
		if strings.TrimSpace(tmp.FilesystemID) == "" {
			return diskBlocked(e, DiskReasonUnavailable)
		}
		e.TempFilesystemID = safeDiskIdentity(tmp.FilesystemID)
		freeBytes, freePercent, freeInodes := capacityMetrics(tmp)
		e.TempFreeBytes, e.TempFreePercent, e.TempFreeInodes = &freeBytes, &freePercent, &freeInodes
		if !capacityMeets(tmp, request.RequiredBytes, request.RequiredInodes, t) {
			return diskBlocked(e, DiskReasonTempVolumeDivergence)
		}
	}
	for _, path := range request.AdditionalPaths {
		if path == "" || path == request.Path || path == request.TempPath {
			continue
		}
		additional, err := backend.StatFS(path)
		if err != nil {
			e.Reason = DiskReasonAdditionalUnavailable
			return diskBlocked(e, DiskReasonAdditionalUnavailable)
		}
		if strings.TrimSpace(additional.FilesystemID) != "" {
			e.FailedFilesystemID = safeDiskIdentity(additional.FilesystemID)
		}
		if validCapacity(additional) != nil {
			e.Reason = DiskReasonAdditionalInvalid
			return diskBlocked(e, DiskReasonAdditionalInvalid)
		}
		freeBytes, freePercent, freeInodes := capacityMetrics(additional)
		e.FailedFreeBytes, e.FailedFreePercent, e.FailedFreeInodes = &freeBytes, &freePercent, &freeInodes
		if strings.TrimSpace(additional.FilesystemID) == "" {
			e.Reason = DiskReasonAdditionalUnavailable
			return diskBlocked(e, DiskReasonAdditionalUnavailable)
		}
		if !capacityMeets(additional, request.RequiredBytes, request.RequiredInodes, t) {
			e.Reason = DiskReasonAdditionalBelow
			return diskBlocked(e, DiskReasonAdditionalBelow)
		}
	}
	e.Reason = DiskReasonNone
	e.NextAction = DiskActionProceed
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
	switch reason {
	case DiskReasonInvalidPolicy:
		e.NextAction = DiskActionFixPolicy
	case DiskReasonInvalidRequest:
		// Retrying the probe cannot fix a caller that supplied no requirement.
		e.NextAction = DiskActionFixPolicy
	case DiskReasonUnavailable, DiskReasonInvalid, DiskReasonAdditionalUnavailable, DiskReasonAdditionalInvalid:
		e.NextAction = DiskActionRetryProbe
	default:
		e.NextAction = DiskActionRecoverSpace
		// FAC-654: attach WHERE the space is. "recover_capacity_without_cleanup"
		// told an operator they were stuck without telling them what to do, and
		// the only large thing they could see was worktrees -- which can hold
		// uncommitted work or an unmerged branch. Reporting rebuildable caches
		// makes the obvious action also the safe one. Best-effort: a scan that
		// finds nothing simply omits the field, and never blocks the refusal.
		if home, hErr := os.UserHomeDir(); hErr == nil {
			e.Reclaimable = ScanReclaimable(home)
		}
	}
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
