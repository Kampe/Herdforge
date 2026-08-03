package deps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProvenanceFence is the machine-readable fence name. Only this JSON is
// authoritative — free-text "depends on FAC-X" is never parsed as authority.
const ProvenanceFence = "herd-deps-v1"

// ParseProvenanceJSON unmarshals versioned structured dependency provenance.
// Rejects unknown versions and malformed edges/holds (no silent drops).
// Empty input is missing provenance (Present=false), not OK.
func ParseProvenanceJSON(raw []byte) (*Provenance, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return &Provenance{Present: false}, fmt.Errorf("%w", ErrMissingProvenance)
	}
	var p Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("deps: structured provenance JSON invalid: %w", err)
	}
	p.Present = true
	if p.Version == 0 {
		return nil, fmt.Errorf("%w: version field required", ErrMissingProvenance)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ExtractProvenanceFromText finds a fenced ```herd-deps-v1 JSON block.
// Does NOT scan free prose for FAC-N mentions.
// No fence => Present=false (missing) — never invent empty OK provenance.
func ExtractProvenanceFromText(text string) (*Provenance, error) {
	const open = "```" + ProvenanceFence
	idx := strings.Index(text, open)
	if idx < 0 {
		trim := strings.TrimSpace(text)
		if strings.HasPrefix(trim, "{") && strings.Contains(trim, `"version"`) && strings.Contains(trim, `"edges"`) {
			return ParseProvenanceJSON([]byte(trim))
		}
		return &Provenance{Present: false}, nil
	}
	rest := text[idx+len(open):]
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		return nil, fmt.Errorf("deps: unclosed %s fence", ProvenanceFence)
	}
	return ParseProvenanceJSON([]byte(rest[:end]))
}

// EmptyProvenance returns an explicit versioned empty record (Present=true)
// for tasks with zero declared dependencies. Still requires board extras check.
func EmptyProvenance(taskRef Ref) *Provenance {
	return &Provenance{
		Version: SchemaVersion,
		TaskRef: taskRef,
		Edges:   []DependencyEdge{},
		Present: true,
	}
}

// FormatProvenanceFence renders provenance as a fenced authoritative block
// suitable for TASK-PACKET.md and lifecycle records.
func FormatProvenanceFence(p *Provenance) string {
	if p == nil {
		p = EmptyProvenance("")
	}
	if p.RecordedAt.IsZero() {
		p.RecordedAt = time.Now().UTC()
	}
	// Do not call Validate here for marshaling output we authored.
	b, err := json.MarshalIndent(struct {
		Version          int              `json:"version"`
		TaskRef          Ref              `json:"task_ref"`
		TaskID           TaskID           `json:"task_id,omitempty"`
		Edges            []DependencyEdge `json:"edges"`
		Holds            []Hold           `json:"holds,omitempty"`
		GraphRevision    string           `json:"graph_revision,omitempty"`
		ProviderRevision string           `json:"provider_revision,omitempty"`
		RecordedAt       time.Time        `json:"recorded_at,omitempty"`
	}{
		Version: p.Version, TaskRef: p.TaskRef, TaskID: p.TaskID,
		Edges: p.Edges, Holds: p.Holds, GraphRevision: p.GraphRevision,
		ProviderRevision: p.ProviderRevision, RecordedAt: p.RecordedAt,
	}, "", "  ")
	if err != nil {
		return fmt.Sprintf("```%s\n{\"version\":%d,\"error\":\"marshal_failed\"}\n```\n", ProvenanceFence, SchemaVersion)
	}
	return fmt.Sprintf("```%s\n%s\n```\n", ProvenanceFence, string(b))
}

// PacketSection returns the TASK-PACKET dependency section text.
func PacketSection(p *Provenance) string {
	var b strings.Builder
	b.WriteString("## Structured Dependencies (authoritative)\n")
	b.WriteString("Machine-readable only. Markdown prose is display-only and is never eligibility authority.\n\n")
	b.WriteString(FormatProvenanceFence(p))
	return b.String()
}
