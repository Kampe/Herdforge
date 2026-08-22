package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FAC-556: consumers were parsing lines such as
// "ADMITTED x.md verdict=PASS reviewer=r sha=abc enqueued=true" to drive bounded
// reactions. Decorative text is not an interface: adding a word to a message
// breaks the consumer, and the fields they need most (refusal reason, veto and
// supersession state) were never in the prose at all.
//
// The emitter below is the ONE place an outcome is reported. Prose and JSON are
// two renderings of the same record, so the structured output can never
// disagree with the human line about the same artifact — and neither is produced
// by re-parsing the other.

// reviewIngestOutcome is one artifact's disposition.
type reviewIngestOutcome struct {
	Artifact string `json:"artifact"`
	Path     string `json:"path,omitempty"`
	// Disposition is the machine-stable name: admitted, duplicate, refused,
	// retired, would_admit, would_skip.
	Disposition string `json:"disposition"`
	SHA         string `json:"sha,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	// Families are omitted when the artifact did not declare them, rather than
	// emitted empty: a consumer checking family disjointness must be able to
	// tell "not stated" from "stated as blank".
	ReviewerFamily string `json:"reviewer_family,omitempty"`
	BuilderFamily  string `json:"builder_family,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	Gate           string `json:"gate,omitempty"`
	Authority      string `json:"authority,omitempty"`
	// Enqueued is a pointer so "not applicable" is distinguishable from false.
	Enqueued *bool `json:"enqueued,omitempty"`
	// Reason carries the refusal cause, which the prose only ever sent to
	// stderr where it could not be correlated with its artifact.
	Reason string `json:"reason,omitempty"`
}

// reviewIngestEmitter renders outcomes as prose or collects them for JSON.
type reviewIngestEmitter struct {
	asJSON   bool
	outcomes []reviewIngestOutcome
}

func (e *reviewIngestEmitter) record(o reviewIngestOutcome, humanLine string, toStderr bool) {
	if e.asJSON {
		e.outcomes = append(e.outcomes, o)
		return
	}
	if toStderr {
		fmt.Fprint(os.Stderr, humanLine)
		return
	}
	fmt.Print(humanLine)
}

// refused reports an artifact that was not admitted, with its cause.
func (e *reviewIngestEmitter) refused(path string, err error) {
	e.record(reviewIngestOutcome{
		Artifact:    filepath.Base(path),
		Path:        path,
		Disposition: "refused",
		Reason:      err.Error(),
	}, fmt.Sprintf("REFUSED %s: %v\n", filepath.Base(path), err), true)
}

// summary closes the batch. In JSON mode this is the only thing written to
// stdout, so the document is always parseable in full.
func (e *reviewIngestEmitter) summary(admitted, refused int) error {
	if !e.asJSON {
		fmt.Printf("herd review-ingest: admitted=%d refused=%d\n", admitted, refused)
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"admitted": admitted,
		"refused":  refused,
		"outcomes": e.outcomes,
	})
}

func boolPtr(b bool) *bool { return &b }
