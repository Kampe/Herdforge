package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// FAC-556: preflight reported per-check results only as prose, so a consumer
// automating a bounded reaction had to match sentences like "Preflight boundary
// check passed." A renamed message silently breaks that, and a failure's cause
// was only ever available as free text.
//
// Each check now records a structured result. Prose stays the default and is
// unchanged; --json renders the same records. Neither is derived from the other.

// preflightCheck is one named check's outcome.
type preflightCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// Detail is the human sentence for a pass, or the failure cause. Present in
	// both cases because the pass text often carries the measured fact.
	Detail string `json:"detail,omitempty"`
	// Warning marks a check that reports a condition without failing the run,
	// so a consumer can distinguish "needs attention" from "blocked".
	Warning bool `json:"warning,omitempty"`
}

// preflightRecorder collects check results and renders one of the two surfaces.
type preflightRecorder struct {
	asJSON bool
	checks []preflightCheck
	failed bool
}

// pass records a successful check.
func (r *preflightRecorder) pass(name, detail string) {
	r.checks = append(r.checks, preflightCheck{Name: name, Passed: true, Detail: detail})
	if !r.asJSON && detail != "" {
		fmt.Println(detail)
	}
}

// fail records a failing check. In prose mode this exits, preserving the
// existing fail-fast behaviour exactly; in JSON mode collection continues so the
// document is complete, and the exit status is applied at the end.
func (r *preflightRecorder) fail(name string, err error) {
	r.checks = append(r.checks, preflightCheck{Name: name, Detail: err.Error()})
	r.failed = true
	if !r.asJSON {
		fmt.Fprintf(os.Stderr, "Preflight failed: %v\n", err)
		os.Exit(1)
	}
}

// warn records a condition that does not fail the run.
func (r *preflightRecorder) warn(name, detail string) {
	r.checks = append(r.checks, preflightCheck{Name: name, Passed: false, Warning: true, Detail: detail})
	if !r.asJSON {
		fmt.Fprintf(os.Stderr, "Preflight WARNING: %s\n", detail)
	}
}

// finish emits the JSON document when requested and applies the exit status.
func (r *preflightRecorder) finish() {
	if !r.asJSON {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"passed": !r.failed,
		"checks": r.checks,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: encode: %v\n", err)
		os.Exit(1)
	}
	if r.failed {
		os.Exit(1)
	}
}
