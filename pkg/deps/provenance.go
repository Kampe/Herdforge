package deps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProvenanceFence is the machine-readable fence name. Only this JSON is
// authoritative — free-text "depends on FAC-X" is never parsed as authority.
const ProvenanceFence = "herd-deps-v1"

// ParseProvenanceJSON unmarshals with DisallowUnknownFields. Rejects unknown
// versions, missing task_ref, and malformed edges/holds. Empty input is missing.
func ParseProvenanceJSON(raw []byte) (*Provenance, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return &Provenance{Present: false}, fmt.Errorf("%w", ErrMissingProvenance)
	}
	// Reject trailing garbage after one JSON value + unknown fields.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Provenance
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("deps: structured provenance JSON invalid: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("deps: trailing JSON after provenance record")
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

// ExtractProvenanceFromText finds exactly one fenced ```herd-deps-v1 JSON block.
// Multiple fences, conflicting content, or trailing junk after the fence fail closed.
// No fence => Present=false (missing) — never invent empty OK provenance.
func ExtractProvenanceFromText(text string) (*Provenance, error) {
	const open = "```" + ProvenanceFence
	// Count openings.
	count := 0
	search := text
	for {
		i := strings.Index(search, open)
		if i < 0 {
			break
		}
		count++
		search = search[i+len(open):]
	}
	if count == 0 {
		trim := strings.TrimSpace(text)
		if strings.HasPrefix(trim, "{") && strings.Contains(trim, `"version"`) && strings.Contains(trim, `"edges"`) {
			return ParseProvenanceJSON([]byte(trim))
		}
		return &Provenance{Present: false}, nil
	}
	if count > 1 {
		return nil, fmt.Errorf("deps: multiple %s fences (want exactly one)", ProvenanceFence)
	}
	idx := strings.Index(text, open)
	rest := text[idx+len(open):]
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		return nil, fmt.Errorf("deps: unclosed %s fence", ProvenanceFence)
	}
	body := rest[:end]
	// After closing fence, only whitespace is allowed (no second JSON payload).
	after := strings.TrimSpace(rest[end+3:])
	if after != "" && strings.Contains(after, "```"+ProvenanceFence) {
		return nil, fmt.Errorf("deps: multiple %s fences", ProvenanceFence)
	}
	// Reject a second bare JSON object after the fence (conflicting authority).
	if strings.HasPrefix(after, "{") && strings.Contains(after, `"edges"`) {
		return nil, fmt.Errorf("deps: conflicting provenance after fence (trailing JSON)")
	}
	return ParseProvenanceJSON([]byte(body))
}

// EmptyProvenance returns an explicit versioned empty record (Present=true)
// for tasks with zero declared dependencies. task_ref and task_id are required
// for launch bind (use EmptyProvenanceBound).
func EmptyProvenance(taskRef Ref) *Provenance {
	return &Provenance{
		Version: SchemaVersion,
		TaskRef: taskRef,
		Edges:   []DependencyEdge{},
		Present: true,
	}
}

// EmptyProvenanceBound includes immutable task_id for launch/migration records.
func EmptyProvenanceBound(taskRef Ref, taskID TaskID) *Provenance {
	p := EmptyProvenance(taskRef)
	p.TaskID = taskID
	return p
}

// BindAndValidate requires Present provenance, exact task_ref match, and a
// non-empty immutable task_id that matches the live card (replay defense).
func (p *Provenance) BindAndValidate(taskRef Ref, taskID TaskID) error {
	if err := p.Validate(); err != nil {
		return err
	}
	want := Ref(strings.TrimSpace(string(taskRef)))
	got := Ref(strings.TrimSpace(string(p.TaskRef)))
	if !got.Valid() {
		return fmt.Errorf("deps: provenance task_ref required")
	}
	if !strings.EqualFold(string(got), string(want)) {
		return fmt.Errorf("deps: provenance task_ref %q does not bind to task %q (replay rejected)", got, want)
	}
	if !p.TaskID.Valid() {
		return fmt.Errorf("deps: provenance task_id required (immutable identity)")
	}
	if !taskID.Valid() {
		return fmt.Errorf("deps: live task id required for bind")
	}
	if p.TaskID != taskID {
		return fmt.Errorf("deps: provenance task_id %q does not bind to immutable id %q", p.TaskID, taskID)
	}
	return nil
}

// FormatProvenanceFence renders provenance as a fenced authoritative block.
func FormatProvenanceFence(p *Provenance) string {
	if p == nil {
		p = EmptyProvenance("")
	}
	if p.RecordedAt.IsZero() {
		p.RecordedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(struct {
		Version          int              `json:"version"`
		TaskRef          Ref              `json:"task_ref"`
		TaskID           TaskID           `json:"task_id,omitempty"`
		Edges            []DependencyEdge `json:"edges"`
		Holds            []Hold           `json:"holds,omitempty"`
		GraphRevision    string           `json:"graph_revision,omitempty"`
		ProviderRevision string           `json:"provider_revision,omitempty"`
		RecordedAt       time.Time        `json:"recorded_at,omitempty"`
		ScopePackages    []string         `json:"scope_packages,omitempty"`
		ScopeFiles       []string         `json:"scope_files,omitempty"`
	}{
		Version: p.Version, TaskRef: p.TaskRef, TaskID: p.TaskID,
		Edges: p.Edges, Holds: p.Holds, GraphRevision: p.GraphRevision,
		ProviderRevision: p.ProviderRevision, RecordedAt: p.RecordedAt,
		ScopePackages: p.ScopePackages, ScopeFiles: p.ScopeFiles,
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

// AppendOrReplaceFence replaces an existing herd-deps-v1 fence or appends one.
// Used by migration apply. Does not parse free-text dependencies.
func AppendOrReplaceFence(description string, p *Provenance) (string, error) {
	if p == nil || !p.Present {
		return "", fmt.Errorf("%w", ErrMissingProvenance)
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	fence := FormatProvenanceFence(p)
	const open = "```" + ProvenanceFence
	idx := strings.Index(description, open)
	if idx < 0 {
		base := strings.TrimRight(description, "\n")
		if base == "" {
			return fence, nil
		}
		return base + "\n\n" + fence, nil
	}
	rest := description[idx+len(open):]
	endRel := strings.Index(rest, "```")
	if endRel < 0 {
		return "", fmt.Errorf("deps: unclosed fence while replacing")
	}
	end := idx + len(open) + endRel + 3
	// Consume trailing newline after fence.
	for end < len(description) && (description[end] == '\n' || description[end] == '\r') {
		end++
	}
	return description[:idx] + fence + description[end:], nil
}
