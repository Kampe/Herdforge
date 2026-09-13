package mergeadmit

import (
	"context"
	"strings"
	"testing"
)

// FAC-831 review 6c93cc2b advisory: a proof route must not be able to reach an
// unbounded command through a value-only probe. LiveState.OriginMain holds an
// ARBITRARY CLOSURE, and cmd/herd shipped two that ran git outside every bound,
// so a silent fallback would let a proof spend commands nothing measured.
//
// The contract is therefore structural: a proof route REQUIRES OriginMainAt and
// refuses without it, naming the field to set. This pins that refusal so the
// fallback cannot be reintroduced as a convenience.
func TestProofRoutesRefuseAValueOnlyOriginProbe(t *testing.T) {
	g := &Gate{RepoDir: t.TempDir(), Live: LiveState{OriginMain: StaticProbe("deadbeef")}}

	_, err := g.currentOriginMain(context.Background(), "origin_main_post_merge")
	if err == nil {
		t.Fatal("a proof route accepted a value-only probe; an arbitrary closure can run unbounded commands")
	}
	if !strings.Contains(err.Error(), "OriginMainAt") {
		t.Fatalf("the refusal does not name the field a caller must set: %v", err)
	}
}

// The context-aware probe is accepted, and a static reading is a legitimate one:
// it starts no process, so it is safe at any allowance. What was removed is the
// hidden context-free EXECUTABLE, not fixed values.
func TestProofRoutesAcceptAStaticContextProbe(t *testing.T) {
	g := &Gate{RepoDir: t.TempDir(), Live: LiveState{OriginMainAt: StaticOriginProbe("  feedface  ")}}

	got, err := g.currentOriginMain(context.Background(), "origin_main_post_merge")
	if err != nil {
		t.Fatalf("a static context-aware reading was refused: %v", err)
	}
	if got != "feedface" {
		t.Fatalf("reading = %q, want the trimmed value", got)
	}
}

// An empty reading is not evidence of an absent condition, and that fail-closed
// rule survives the move to the context-aware probe.
func TestProofRoutesRefuseAnEmptyOriginReading(t *testing.T) {
	g := &Gate{RepoDir: t.TempDir(), Live: LiveState{OriginMainAt: StaticOriginProbe("   ")}}

	if _, err := g.currentOriginMain(context.Background(), "origin_main_post_merge"); err == nil {
		t.Fatal("an empty integration tip was accepted as a reading")
	}
}
