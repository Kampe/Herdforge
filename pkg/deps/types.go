// Package deps enforces packet↔board dependency-graph conformance (FAC-159).
//
// Authority is structured provenance + provider relation readback — never
// free-text Markdown or comments. Every production launch path must call the
// pre-side-effect gate before worktree, board status, comment, or tab mutation.
package deps

import (
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the current structured dependency provenance version.
const SchemaVersion = 1

// EdgeType is a provider-neutral dependency relation kind.
// Only Blocks is authoritative for eligibility; Related is recorded but never
// unlocks or blocks claim by itself.
type EdgeType string

const (
	EdgeBlocks  EdgeType = "blocks"
	EdgeRelated EdgeType = "related"
)

// TaskID is an immutable provider task identifier (opaque board ID, not ref).
// Empty TaskID is invalid for mutations.
type TaskID string

// String returns the raw ID.
func (id TaskID) String() string { return string(id) }

// Valid reports whether the ID is non-empty after trim.
func (id TaskID) Valid() bool { return strings.TrimSpace(string(id)) != "" }

// Ref is a human ticket ref (e.g. FAC-159). Immutable value type.
type Ref string

func (r Ref) String() string { return string(r) }

func (r Ref) Valid() bool { return strings.TrimSpace(string(r)) != "" }

// DependencyEdge is one directed relation. For EdgeBlocks, Source blocks Target
// (Source is a prerequisite of Target).
type DependencyEdge struct {
	// RelationID is the provider relation id when known (empty for desired-only).
	RelationID string `json:"relation_id,omitempty"`
	// SourceID / TargetID are immutable provider task IDs when resolved.
	SourceID TaskID `json:"source_id,omitempty"`
	TargetID TaskID `json:"target_id,omitempty"`
	// SourceRef / TargetRef are ticket refs (FAC-N). Required for provenance.
	SourceRef Ref      `json:"source_ref"`
	TargetRef Ref      `json:"target_ref"`
	Type      EdgeType `json:"type"`
}

// Key returns a stable identity for set operations: type|sourceRef|targetRef.
func (e DependencyEdge) Key() string {
	return fmt.Sprintf("%s|%s|%s", e.Type, e.SourceRef, e.TargetRef)
}

// ReverseKey is type|targetRef|sourceRef (detects reversed edges).
func (e DependencyEdge) ReverseKey() string {
	return fmt.Sprintf("%s|%s|%s", e.Type, e.TargetRef, e.SourceRef)
}

// HoldKind classifies a durable queue hold that must exist as a board edge.
type HoldKind string

const (
	// HoldExplicit is an explicit prerequisite hold.
	HoldExplicit HoldKind = "explicit"
	// HoldCollisionOwnership is a shared-source-file overlap hold between cards.
	HoldCollisionOwnership HoldKind = "collision_ownership"
)

// Hold is a structured declaration that dependent must wait on blocker.
// Holds never come from Markdown; they are versioned structured input.
type Hold struct {
	Kind          HoldKind `json:"kind"`
	BlockerRef    Ref      `json:"blocker_ref"`
	DependentRef  Ref      `json:"dependent_ref"`
	Paths         []string `json:"paths,omitempty"`
	Note          string   `json:"note,omitempty"`
}

// ToEdge converts a hold into a desired blocks edge (blocker → dependent).
func (h Hold) ToEdge() DependencyEdge {
	return DependencyEdge{
		SourceRef: h.BlockerRef,
		TargetRef: h.DependentRef,
		Type:      EdgeBlocks,
	}
}

// Provenance is versioned structured dependency authority for packets/lifecycle.
// Never invent edges from prose. Missing provenance is explicit empty, not free-text parse.
type Provenance struct {
	Version   int              `json:"version"`
	TaskRef   Ref              `json:"task_ref"`
	TaskID    TaskID           `json:"task_id,omitempty"`
	Edges     []DependencyEdge `json:"edges"`
	Holds     []Hold           `json:"holds,omitempty"`
	// GraphRevision binds the provider relation snapshot used to author this record.
	GraphRevision string    `json:"graph_revision,omitempty"`
	RecordedAt    time.Time `json:"recorded_at,omitempty"`
}

// Normalize sets schema version and drops empty edges.
func (p *Provenance) Normalize() {
	if p.Version == 0 {
		p.Version = SchemaVersion
	}
	out := p.Edges[:0]
	for _, e := range p.Edges {
		e.SourceRef = Ref(strings.TrimSpace(string(e.SourceRef)))
		e.TargetRef = Ref(strings.TrimSpace(string(e.TargetRef)))
		e.Type = EdgeType(strings.ToLower(strings.TrimSpace(string(e.Type))))
		if e.Type == "" {
			e.Type = EdgeBlocks
		}
		if e.SourceRef.Valid() && e.TargetRef.Valid() {
			out = append(out, e)
		}
	}
	p.Edges = out
}

// DesiredBlocks returns desired blocks edges including holds expanded to edges.
func (p Provenance) DesiredBlocks() []DependencyEdge {
	p.Normalize()
	seen := map[string]bool{}
	var out []DependencyEdge
	for _, e := range p.Edges {
		if e.Type != EdgeBlocks {
			continue
		}
		k := e.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	for _, h := range p.Holds {
		e := h.ToEdge()
		k := e.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

// DriftClass is a stable reconciliation finding class.
type DriftClass string

const (
	DriftMissing    DriftClass = "missing"
	DriftExtra      DriftClass = "extra"
	DriftDuplicate  DriftClass = "duplicate"
	DriftReversed   DriftClass = "reversed"
	DriftUnresolved DriftClass = "unresolved"
	DriftDeleted    DriftClass = "deleted"
	DriftCyclic     DriftClass = "cyclic"
)

// Finding is one stable reconcile finding with exact direction + IDs.
type Finding struct {
	Class     DriftClass     `json:"class"`
	Edge      DependencyEdge `json:"edge"`
	Detail    string         `json:"detail,omitempty"`
	RelationID string        `json:"relation_id,omitempty"`
}

// ReconcileReport is stable JSON describing packet vs board edge drift.
type ReconcileReport struct {
	TaskRef       Ref       `json:"task_ref"`
	GraphRevision string    `json:"graph_revision"`
	OK            bool      `json:"ok"`
	Findings      []Finding `json:"findings"`
	// ManagedBoard is the set of blocks edges the board holds that involve TaskRef.
	ManagedBoard []DependencyEdge `json:"managed_board"`
	// Desired is the structured desired set (packet/holds).
	Desired []DependencyEdge `json:"desired"`
}

// BlockedError is a typed fail-closed launch block (nonzero exit).
type BlockedError struct {
	Ref     Ref
	Reason  string
	Code    string // e.g. open_blocker, capability, drift, cyclic, toctou, stale
	Details []string
	Report  *ReconcileReport
}

func (e *BlockedError) Error() string {
	if e == nil {
		return "deps: blocked"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "deps: BLOCKED %s code=%s: %s", e.Ref, e.Code, e.Reason)
	for _, d := range e.Details {
		fmt.Fprintf(&b, "\n  - %s", d)
	}
	return b.String()
}

// GraphRevision hashes a stable snapshot of edges + task status map for TOCTOU.
func GraphRevision(edges []DependencyEdge, statusByRef map[string]string) string {
	// Deterministic: sort keys then FNV-ish string join (no crypto needed).
	keys := make([]string, 0, len(edges)+len(statusByRef))
	for _, e := range edges {
		keys = append(keys, e.Key()+"#"+e.RelationID)
	}
	for ref, st := range statusByRef {
		keys = append(keys, "status|"+ref+"|"+st)
	}
	// Insertion sort for zero-dep small N.
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}
	h := uint64(2166136261)
	for _, k := range keys {
		for i := 0; i < len(k); i++ {
			h ^= uint64(k[i])
			h *= 16777619
		}
		h ^= 0xff
		h *= 16777619
	}
	return fmt.Sprintf("%016x", h)
}
