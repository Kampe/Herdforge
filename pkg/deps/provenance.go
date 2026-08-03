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
// Rejects unknown versions and refuses to invent edges from non-JSON text.
func ParseProvenanceJSON(raw []byte) (*Provenance, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return &Provenance{Version: SchemaVersion}, nil
	}
	var p Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("deps: structured provenance JSON invalid: %w", err)
	}
	if p.Version == 0 {
		p.Version = SchemaVersion
	}
	if p.Version != SchemaVersion {
		return nil, fmt.Errorf("deps: unsupported provenance version %d (want %d)", p.Version, SchemaVersion)
	}
	p.Normalize()
	return &p, nil
}

// ExtractProvenanceFromText finds a fenced ```herd-deps-v1 JSON block.
// Does NOT scan free prose for FAC-N mentions. No fence => empty provenance.
func ExtractProvenanceFromText(text string) (*Provenance, error) {
	const open = "```" + ProvenanceFence
	idx := strings.Index(text, open)
	if idx < 0 {
		// Also accept bare JSON object with "version" + "edges" keys only when
		// the entire trimmed text is JSON — still not Markdown inference.
		trim := strings.TrimSpace(text)
		if strings.HasPrefix(trim, "{") && strings.Contains(trim, `"edges"`) {
			return ParseProvenanceJSON([]byte(trim))
		}
		return &Provenance{Version: SchemaVersion}, nil
	}
	rest := text[idx+len(open):]
	// Optional newline after fence tag
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		return nil, fmt.Errorf("deps: unclosed %s fence", ProvenanceFence)
	}
	return ParseProvenanceJSON([]byte(rest[:end]))
}

// FormatProvenanceFence renders provenance as a fenced authoritative block
// suitable for TASK-PACKET.md and lifecycle records.
func FormatProvenanceFence(p *Provenance) string {
	if p == nil {
		p = &Provenance{Version: SchemaVersion}
	}
	p.Normalize()
	if p.RecordedAt.IsZero() {
		p.RecordedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		// Fail closed: never emit partial authority.
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
