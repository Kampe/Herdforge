package graph

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GraphStatusReport is the subset of `code-review-graph status --json` used
// to decide freshness. Extra fields are ignored.
type GraphStatusReport struct {
	BuiltAtCommit string `json:"built_at_commit"`
	// CurrentSHA is the revision the working tree is on right now. It differs
	// from BuiltAtCommit whenever the tree moved after the last index run.
	CurrentSHA string `json:"current_sha"`
	Nodes      int    `json:"nodes"`
	Edges      int    `json:"edges"`
	Files      int    `json:"files"`
}

// ParseGraphStatusJSON decodes a code-review-graph status --json body.
// A null or empty built_at_commit is valid and means the graph is unbuilt.
func ParseGraphStatusJSON(raw []byte) (GraphStatusReport, error) {
	var r GraphStatusReport
	if len(strings.TrimSpace(string(raw))) == 0 {
		return r, fmt.Errorf("graph status JSON is empty")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("decode graph status: %w", err)
	}
	r.BuiltAtCommit = strings.TrimSpace(r.BuiltAtCommit)
	r.CurrentSHA = strings.TrimSpace(r.CurrentSHA)
	return r, nil
}

// EvidenceFromStatus builds GraphEvidence from a status report and optional
// pre-collected hits. Callers still own querying tests_for/importers_of.
func EvidenceFromStatus(status GraphStatusReport, hits []GraphHit) GraphEvidence {
	cp := append([]GraphHit(nil), hits...)
	return GraphEvidence{
		BuiltAtCommit: status.BuiltAtCommit,
		Hits:          cp,
	}
}
