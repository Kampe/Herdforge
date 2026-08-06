package shot

import (
	"strings"
	"testing"
)

// codex exec is sandboxed with networking off, so a research shot routed there
// returns confident nonsense rather than an error.
func TestNetworkShapeExcludesSandboxedCodex(t *testing.T) {
	r := Request{Shape: "research"}
	if !r.NeedsNetwork() {
		t.Fatal("research needs network")
	}
	for _, p := range r.Eligible() {
		if p == "codex" {
			t.Fatal("codex must be excluded from a network-requiring shot")
		}
	}
	if !contains(r.Excluded(), "codex") {
		t.Fatal("codex must appear in the exclusion list")
	}
	// A bounded shot has no such restriction.
	if !contains(Request{Shape: "bounded"}.Eligible(), "codex") {
		t.Fatal("codex is fine for an offline bounded shot")
	}
}

func TestSchemaRestrictsToStructuredOutputSurfaces(t *testing.T) {
	r := Request{Shape: "bounded", Schema: "s.json"}
	got := r.Eligible()
	if len(got) != 2 || !contains(got, "codex") || !contains(got, "grok") {
		t.Fatalf("schema shots must route only to codex/grok, got %v", got)
	}
}

// Refusing a structurally impossible pin beats routing into garbage that
// exits zero.
func TestPinIsRefusedWhenItCannotDoTheJob(t *testing.T) {
	if err := (Request{Shape: "bounded", Schema: "s.json", Provider: "claude"}).ValidatePin(); err == nil {
		t.Fatal("schema on a non-JSON surface must be refused")
	}
	if err := (Request{Shape: "research", Provider: "codex"}).ValidatePin(); err == nil {
		t.Fatal("sandboxed codex on a network shot must be refused")
	}
	if err := (Request{Shape: "bounded", Provider: "nope"}).ValidatePin(); err == nil {
		t.Fatal("an unknown surface must be refused")
	}
	if err := (Request{Shape: "research", Provider: "grok"}).ValidatePin(); err != nil {
		t.Fatalf("a capable pin must be accepted: %v", err)
	}
	if err := (Request{Shape: "bounded"}).ValidatePin(); err != nil {
		t.Fatalf("no pin is always fine: %v", err)
	}
}

// A thin or empty answer that exits zero is the failure this guards.
func TestOutputValidationCatchesNonSubstantiveResults(t *testing.T) {
	research := Request{Shape: "research"}
	if err := research.ValidateOutput("   "); err == nil {
		t.Fatal("an empty body must fail")
	}
	if err := research.ValidateOutput("looks fine to me"); err == nil {
		t.Fatal("a thin report must fail the min-length floor")
	}
	if err := research.ValidateOutput(strings.Repeat("x", MinReportChars)); err != nil {
		t.Fatalf("a substantive report must pass: %v", err)
	}
	// A bounded shot has no report floor: a one-word answer can be correct.
	if err := (Request{Shape: "bounded"}).ValidateOutput("42"); err != nil {
		t.Fatalf("bounded shots have no length floor: %v", err)
	}
}

func TestSchemaShotMustReturnValidJSON(t *testing.T) {
	r := Request{Shape: "bounded", Schema: "s.json"}
	if err := r.ValidateOutput("not json at all"); err == nil {
		t.Fatal("a schema shot returning prose must fail")
	}
	if err := r.ValidateOutput(`{"ok":true}`); err != nil {
		t.Fatalf("valid JSON must pass: %v", err)
	}
	// Short JSON is fine — the length floor must not apply to schema shots.
	if err := r.ValidateOutput(`[]`); err != nil {
		t.Fatalf("short valid JSON must pass: %v", err)
	}
}

func TestEligibleIsDeterministic(t *testing.T) {
	r := Request{Shape: "research"}
	first := strings.Join(r.Eligible(), ",")
	for i := 0; i < 5; i++ {
		if got := strings.Join(r.Eligible(), ","); got != first {
			t.Fatalf("eligible set must be stable: %q vs %q", got, first)
		}
	}
}
