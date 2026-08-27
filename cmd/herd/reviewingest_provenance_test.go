package main

import (
	"os"
	"strings"
	"testing"
)

// FAC-620: the PRODUCTION-PATH test the veto required.
//
// This card was FAILed twice for the same shape: a correct helper with no
// production caller. `rg` proved ReconcileBuilderFamily appeared only in its own
// package and test, so builder family still arrived as whatever a reviewer
// typed.
//
// This asserts the call exists on the shipped ingest path and FAILS if it is
// removed. It reads the source deliberately: the alternative is standing up a
// full review-ingest run with a git repo, a receipt store and a ledger, and a
// test that heavy would have been skipped -- which is how the gap survived.
//
// Source-level, but not vacuous: deleting the production call makes it red,
// which is exactly the property that was missing.
func TestReconciliationIsCalledFromTheProductionIngestPath(t *testing.T) {
	src, err := os.ReadFile("reviewingest.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "reviewingest.ReconcileBuilderFamilyForSHA(") {
		t.Fatal("runReviewIngest does not call ReconcileBuilderFamilyForSHA. " +
			"A reconciliation with no production caller leaves builder-family as whatever a reviewer typed -- " +
			"the helper-only defect this card was FAILed for twice.")
	}

	// It must run BEFORE Validate: a family reconciled after validation cannot
	// influence the independence check that Validate performs.
	call := strings.Index(body, "reviewingest.ReconcileBuilderFamilyForSHA(")
	validate := strings.Index(body, "a.Validate(coordinators, commitExists)")
	if validate < 0 {
		t.Fatal("could not locate the Validate call to order against")
	}
	if call > validate {
		t.Fatal("reconciliation runs AFTER Validate; the independence check would use the reviewer's " +
			"claimed family rather than recorded provenance")
	}

	// It must pass real git reachability, not a branch-name match.
	if !strings.Contains(body, "branchReachesSHA") {
		t.Fatal("reconciliation is not given a reachability proof; branch text alone is not evidence " +
			"that a receipt describes the reviewed commit")
	}
}

// The reachability helper must fail closed: an unknown branch or sha is not
// reachability, and treating a git error as proof would resurrect the defect.
func TestBranchReachesSHAFailsClosedOnUnknownInputs(t *testing.T) {
	if branchReachesSHA("", "deadbeef") {
		t.Fatal("empty branch reported as reaching a commit")
	}
	if branchReachesSHA("wt/x", "") {
		t.Fatal("empty sha reported as reachable")
	}
	if branchReachesSHA("definitely-not-a-branch-xyz", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatal("a nonexistent branch reported as reaching a commit; a git failure must not read as proof")
	}
}
