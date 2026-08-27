package reviewingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The reviewed commit was written at 20:00. Every receipt below is placed
// relative to it deliberately.
var commitAt = time.Date(2026, 8, 27, 20, 0, 0, 0, time.UTC)

// noLedger: the ledger can prove nothing. notCandid: the artifact made a bare
// claim. Together they are the strictest setting, so any test using them that
// does NOT expect a refusal is asserting the receipt join carried the decision.
func noLedger(string) (string, error) { return "", nil }
func notCandid(string) bool          { return false }
func isCandid(string) bool           { return true }

const claudeRow = `{"created_at":"2026-08-27T19:00:00Z","task_ref":"defi-crusader","lane":"defi-crusader","provider":"claude","model":"claude-sonnet-5","builder_family":"anthropic","branch":"wt/defi-crusader","accepted":true}`

// An artifact that states nothing gets its family FROM provenance, instead of a
// reviewer guessing at a route it never observed.
func TestAnUnstatedFamilyIsFilledFromRecordedProvenance(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt, func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
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

	err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt, func(b, sha string) bool { return b == "wt/defi-crusader" })
	if err == nil {
		t.Fatal("a builder-family contradicting recorded provenance was admitted; " +
			"independence would be computed against a family that never wrote the code")
	}
	if !strings.Contains(err.Error(), "contradicts launch provenance") {
		t.Fatalf("refusal does not name the conflict: %v", err)
	}
}

// Agreement is not a conflict.
func TestAMatchingFamilyIsAccepted(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{BuilderFamily: "anthropic"}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt, func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatalf("a family agreeing with provenance was refused: %v", err)
	}
}

// No recorded provenance must NOT be treated as a CONTRADICTION. Absence of a
// receipt is the pre-FAC-620 state, not evidence of a different builder.
// A candid artifact passes through untouched.
func TestNoRecordedProvenanceLeavesACandidArtifactAlone(t *testing.T) {
	path := receiptFile(t, `{"branch":"wt/other","provider":"grok","builder_family":"xai","accepted":true}`)
	a := &Artifact{BuilderFamily: "unknown"}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt,
		func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatalf("absence of a receipt was treated as a conflict: %v", err)
	}
	if a.BuilderFamily != "unknown" {
		t.Fatalf("artifact was mutated with no provenance to justify it: %q", a.BuilderFamily)
	}
}

// A relaunched branch: the LAST accepted receipt produced the current tip.
func TestTheMostRecentLaunchForABranchWins(t *testing.T) {
	path := receiptFile(t,
		`{"created_at":"2026-08-27T18:00:00Z","branch":"wt/defi-crusader","provider":"codex","builder_family":"openai","accepted":true}`,
		claudeRow,
	)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt, func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q; an earlier launch outranked the one that produced the tip", a.BuilderFamily)
	}
}

// FAC-620: branch TEXT is not evidence. A receipt whose branch no longer
// reaches the reviewed commit must be ignored, not used -- branches are reused,
// relaunched and rebased, and a confidently wrong family is worse than none.
func TestAReceiptWhoseBranchDoesNotReachTheSHAIsIgnored(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{BuilderFamily: "openai"}

	// The branch exists in the receipt but does NOT contain this commit. The
	// ledger independently proves openai, so the only question under test is
	// whether the unreachable receipt overwrites it.
	err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt,
		func(branch, sha string) bool { return false })

	if err != nil {
		t.Fatalf("an unreachable branch was treated as contradicting provenance: %v", err)
	}
	if a.BuilderFamily != "openai" {
		t.Fatalf("family was overwritten from a receipt that does not reach the commit: %q", a.BuilderFamily)
	}
}

// Unknown reachability must never read as proven. A git failure is not evidence.
func TestUnknownReachabilityIsNotProvenance(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt, nil); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "" {
		t.Fatalf("family %q was resolved with no way to prove reachability", a.BuilderFamily)
	}
}

// THE third-review regression. Standing lanes are relaunched on the SAME
// branch as normal operation, so a launch that happened AFTER a commit was
// written still reaches it. Reachability alone handed that commit to the later
// launch -- an agent that had not started when the code was authored.
//
//	19:00 launch A (anthropic) -> 20:00 A commits -> 21:00 relaunch B (xai)
//
// Both receipts reach the commit. Only A can have produced it.
func TestALaunchAfterTheCommitDoesNotStealAttribution(t *testing.T) {
	path := receiptFile(t,
		claudeRow, // 19:00, anthropic -- the launch that was running
		`{"created_at":"2026-08-27T21:00:00Z","branch":"wt/defi-crusader","provider":"grok","builder_family":"xai","accepted":true}`,
	)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt,
		func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q, want anthropic. A relaunch at 21:00 was credited with a commit "+
			"written at 20:00; that agent had not started when the code was authored.", a.BuilderFamily)
	}
}

// And the same steal must not be laundered into a REFUSAL either: a correct
// stated family must not be rejected because a later relaunch disagrees.
func TestACorrectStatedFamilyIsNotRefusedByALaterRelaunch(t *testing.T) {
	path := receiptFile(t,
		claudeRow,
		`{"created_at":"2026-08-27T21:00:00Z","branch":"wt/defi-crusader","provider":"grok","builder_family":"xai","accepted":true}`,
	)
	a := &Artifact{BuilderFamily: "anthropic"} // correct, and provable

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt,
		func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatalf("a truthful builder-family was refused because a LATER launch reused the branch: %v", err)
	}
}

// An unknown commit time cannot order anything, so it is not provenance. It
// must not fall back to reachability alone -- that is the defect above.
func TestAnUnknownCommitTimeYieldsNoProvenance(t *testing.T) {
	path := receiptFile(t, claudeRow)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", time.Time{},
		func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "" {
		t.Fatalf("family %q resolved with no commit time to order the receipts against", a.BuilderFamily)
	}
}

// A receipt with no timestamp cannot be proven to predate the commit.
func TestAnUndatedReceiptIsNotProvenance(t *testing.T) {
	path := receiptFile(t, `{"branch":"wt/defi-crusader","provider":"grok","builder_family":"xai","accepted":true}`)
	a := &Artifact{}

	if err := ReconcileBuilderFamilyForSHA(a, path, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", commitAt,
		func(b, sha string) bool { return b == "wt/defi-crusader" }); err != nil {
		t.Fatal(err)
	}
	if a.BuilderFamily != "" {
		t.Fatalf("family %q resolved from an undated receipt", a.BuilderFamily)
	}
}

// THE fourth-review regression. Every rejection path in the join lands on "no
// qualifying receipt", and that used to leave the artifact ALONE -- so each
// time the join got stricter, MORE reviewer-asserted families went unchecked.
// Tightening the guard widened the hole it existed to close.
//
// A bare assertion with no receipt and no ledger record must be refused.
func TestABareAssertedFamilyWithNoCorroborationIsDowngraded(t *testing.T) {
	a := &Artifact{BuilderFamily: "openai"} // asserted, nothing behind it

	if err := RequireCorroboratedFamily(a, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "unrecorded", noLedger, notCandid); err != nil {
		t.Fatalf("an uncorroborated family was REFUSED rather than downgraded: %v.\n"+
			"Refusing halts every first review: the ledger record that could corroborate a SHA "+
			"is created by ingesting a verdict for that SHA.", err)
	}
	if a.BuilderFamily != "unrecorded" {
		t.Fatalf("builder_family = %q, want unrecorded. An unproven assertion stayed TRUSTED, so "+
			"independence would be computed against a family nothing proves -- the hole FAC-620 exists to close.",
			a.BuilderFamily)
	}
}

// The escape hatch must stay open: a reviewer who candidly says "unknown" is
// being honest, and the ingest gate routes that to provenance-unrecorded.
// Refusing it would punish honesty and reward assertion -- the FAC-608 defect.
func TestACandidUnrecordedFamilyIsStillAdmitted(t *testing.T) {
	a := &Artifact{BuilderFamily: "unknown"}

	if err := RequireCorroboratedFamily(a, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "unrecorded", noLedger, isCandid); err != nil {
		t.Fatalf("a candid unrecorded family was refused: %v", err)
	}
	if a.BuilderFamily != "unknown" {
		t.Fatalf("a candid family was rewritten to %q; the honest case must pass through untouched", a.BuilderFamily)
	}
}

// A ledger record for the EXACT SHA is corroboration, and admits the claim.
func TestALedgerProvenFamilyCorroboratesTheAssertion(t *testing.T) {
	a := &Artifact{BuilderFamily: "openai"}

	if err := RequireCorroboratedFamily(a, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "unrecorded",
		func(string) (string, error) { return "openai", nil }, notCandid); err != nil {
		t.Fatalf("a ledger-proven family was refused: %v", err)
	}
	if a.BuilderFamily != "openai" {
		t.Fatalf("a ledger-PROVEN family was downgraded to %q; corroboration must preserve it", a.BuilderFamily)
	}
}

// And a claim the ledger CONTRADICTS is refused, not quietly overwritten.
func TestALedgerContradictingTheAssertionRefuses(t *testing.T) {
	a := &Artifact{BuilderFamily: "openai"}

	err := RequireCorroboratedFamily(a, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "unrecorded",
		func(string) (string, error) { return "anthropic", nil }, notCandid)
	if err == nil || !strings.Contains(err.Error(), "contradicts ledger-recorded") {
		t.Fatalf("a family contradicting the ledger was not refused: %v", err)
	}
}

// An unreadable ledger is not proof of absence. It must refuse rather than
// admit -- reporting "nothing proves this" when the check itself failed is the
// absence-as-definitive-negative defect this whole card is made of.
func TestAnUnreadableLedgerRefusesRatherThanAdmitting(t *testing.T) {
	a := &Artifact{BuilderFamily: "openai"}

	err := RequireCorroboratedFamily(a, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "unrecorded",
		func(string) (string, error) { return "", errors.New("ledger unreadable") }, notCandid)
	if err == nil || !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("an unreadable ledger was treated as proof of absence: %v", err)
	}
}
