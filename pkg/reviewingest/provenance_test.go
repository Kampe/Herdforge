package reviewingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-620 P1: the writer being correct proves nothing about whether any
// consumer reads it. The first version of this card shipped a sound writer with
// no propagation and was FAILed for exactly that.
//
// These drive ReconcileBuilderFamily -- the intake path that joins an artifact
// to recorded provenance -- and each fails if that join is removed.

func receiptFile(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "launch-receipts.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const claudeRow = `{"created_at":"2026-08-27T19:00:00Z","task_ref":"defi-crusader","lane":"defi-crusader","provider":"claude","model":"claude-sonnet-5","builder_family":"anthropic","branch":"wt/defi-crusader","accepted":true}`

// An artifact that states nothing gets its family FROM provenance, instead of a
// reviewer guessing at a route it never observed.
func TestAnUnstatedFamilyIsFilledFromRecordedProvenance(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{}

	if err := ReconcileBuilderFamily(a, path, "wt/defi-crusader"); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q, want anthropic resolved from the launch receipt", a.BuilderFamily)
	}
}

// THE dangerous case. A reviewer attesting codex for a branch claude actually
// built must be refused, not admitted.
func TestAStatedFamilyContradictingProvenanceIsRefused(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{BuilderFamily: "openai"} // reviewer believed the configured pin

	err := ReconcileBuilderFamily(a, path, "wt/defi-crusader")
	if err == nil {
		t.Fatal("a builder-family contradicting recorded provenance was admitted; " +
			"independence would be computed against a family that never wrote the code")
	}
	if !strings.Contains(err.Error(), "contradicts recorded launch provenance") {
		t.Fatalf("refusal does not name the conflict: %v", err)
	}
}

// Agreement is not a conflict.
func TestAMatchingFamilyIsAccepted(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{BuilderFamily: "anthropic"}

	if err := ReconcileBuilderFamily(a, path, "wt/defi-crusader"); err != nil {
		t.Fatalf("a family agreeing with provenance was refused: %v", err)
	}
}

// No recorded provenance must NOT refuse. Absence of a receipt is the
// pre-FAC-620 state, not evidence of a different builder -- refusing on it
// would reject every historical artifact.
func TestNoRecordedProvenanceLeavesTheArtifactAlone(t *testing.T) {
	path := receiptFile(t, `{"branch":"wt/other","provider":"grok","builder_family":"xai","accepted":true}`)
	a := &Artifact{BuilderFamily: "openai"}

	if err := ReconcileBuilderFamily(a, path, "wt/defi-crusader"); err != nil {
		t.Fatalf("absence of a receipt was treated as a conflict: %v", err)
	}
	if a.BuilderFamily != "openai" {
		t.Fatalf("artifact was mutated with no provenance to justify it: %q", a.BuilderFamily)
	}
}

// A relaunched branch: the LAST accepted receipt produced the current tip.
func TestTheMostRecentLaunchForABranchWins(t *testing.T) {
	path := receiptFile(t,
		`{"branch":"wt/defi-crusader","provider":"codex","builder_family":"openai","accepted":true}`,
		claudeRow,
	)
	a := &Artifact{}

	if err := ReconcileBuilderFamily(a, path, "wt/defi-crusader"); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q; an earlier launch outranked the one that produced the tip", a.BuilderFamily)
	}
}
