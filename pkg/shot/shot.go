// Package shot ports bin/herd-shot: run ONE bounded task headless through the
// quota router and return the result.
//
// No tab, no pane, no session bootstrap. This is the cheap lane for recon, QA
// questions, and structured extractions that do not deserve a full agent: a
// pane pays hooks plus CLAUDE.md plus a kickoff on every spawn, a shot pays
// only the prompt.
//
// The capability gates matter more than they look. A shot routed to a surface
// that structurally cannot do the job does not fail cleanly — it returns
// plausible garbage, and the exit code lies about it.
package shot

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/router"
)

// DefaultShape is the cheap read-only lane.
const DefaultShape = "bounded"

// MinReportChars is the floor below which a report-shaped answer is treated as
// non-substantive. A three-word reply to a research shot is a failure wearing
// a zero exit code.
const MinReportChars = 200

// ShotProviders are the surfaces a headless shot can use at all. Derived from
// the router's headless surface catalog (router.HeadlessProviders) so the two
// lists can never disagree: a surface the router can route and launch
// headlessly is shot-capable, full stop. Before FAC-224 this was a second
// hardcoded list that silently omitted kimi, so under quota pressure the
// router's own recommendation could not be executed.
var ShotProviders = router.HeadlessProviders()

// SchemaProviders support constrained structured output.
var SchemaProviders = []string{"codex", "grok"}

// NetworkShapes need to reach the network to produce a real answer.
var NetworkShapes = []string{"research"}

// ReportShapes must return a substantive body, not an acknowledgement.
var ReportShapes = []string{"research", "architecture"}

// ShotNetworkCapable are surfaces that reach the network IN SHOT MODE.
//
// codex is deliberately absent: `codex exec` runs sandboxed with networking
// disabled, so a codex shot on a network-requiring shape returns confident
// nonsense rather than an error. Its interactive pane does have network, which
// is exactly why this list is separate from general provider capability.
var ShotNetworkCapable = []string{"claude", "agy", "ollama", "grok", "lazer"}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// Request describes one shot.
type Request struct {
	Shape    string
	Provider string // explicit pin, optional
	Schema   string // path to a JSON Schema, optional
	Prompt   string
}

// NeedsNetwork reports whether this shape cannot be answered offline.
func (r Request) NeedsNetwork() bool { return contains(NetworkShapes, r.Shape) }

// NeedsReport reports whether the answer must be substantive.
func (r Request) NeedsReport() bool {
	return r.Schema != "" || contains(ReportShapes, r.Shape)
}

// Eligible returns the providers that can structurally do this job, sorted for
// determinism.
func (r Request) Eligible() []string {
	avail := append([]string(nil), ShotProviders...)
	if r.Schema != "" {
		avail = append([]string(nil), SchemaProviders...)
	}
	if r.NeedsNetwork() {
		var keep []string
		for _, p := range avail {
			if contains(ShotNetworkCapable, p) {
				keep = append(keep, p)
			}
		}
		avail = keep
	}
	sort.Strings(avail)
	return avail
}

// ValidatePin refuses an explicit --provider that cannot do the job, rather
// than routing into garbage that exits zero.
func (r Request) ValidatePin() error {
	if r.Provider == "" {
		return nil
	}
	if r.Schema != "" && !contains(SchemaProviders, r.Provider) {
		return fmt.Errorf("--schema requires one of %s, pinned %s",
			strings.Join(SchemaProviders, "/"), r.Provider)
	}
	if r.NeedsNetwork() && !contains(ShotNetworkCapable, r.Provider) {
		return fmt.Errorf("--provider %s cannot reach the network for a %q shot; "+
			"codex exec is sandboxed with networking disabled — pick one of %s",
			r.Provider, r.Shape, strings.Join(ShotNetworkCapable, ","))
	}
	if !contains(ShotProviders, r.Provider) {
		return fmt.Errorf("--provider %s is not a shot-capable surface", r.Provider)
	}
	return nil
}

// Excluded returns the providers to keep the router away from. Excluding is
// deliberate rather than allowlisting: a real auth or catalog gate must still
// apply on top, and an allowlist would paper over it.
func (r Request) Excluded() []string {
	eligible := r.Eligible()
	var out []string
	for _, p := range ShotProviders {
		if !contains(eligible, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateOutput checks that a result is substantive. Without this, a thin or
// empty answer exits zero and the caller believes it.
//
// A garbage result is NOT evidence of a dead provider, so callers must never
// cool a surface on this error — they reroute once and then fail.
func (r Request) ValidateOutput(body string) error {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return fmt.Errorf("shot returned an empty body")
	}
	if r.Schema != "" {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
			return fmt.Errorf("schema shot did not return valid JSON: %w", err)
		}
		return nil
	}
	if r.NeedsReport() && len(trimmed) < MinReportChars {
		return fmt.Errorf("shot returned %d chars, below the %d-char floor for a %q shot",
			len(trimmed), MinReportChars, r.Shape)
	}
	return nil
}
