package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUnattestableFieldsAreAbsentNotZero is the FAC-556 contract.
//
// A consumer automating a bounded reaction must be able to tell "Herdforge does
// not know this" from "the value is empty". Emitting a plausible zero collapses
// those, and the consumer acts on a fact nobody attested.
func TestUnattestableFieldsAreAbsentNotZero(t *testing.T) {
	// A refused artifact has no verdict, no families, no enqueue state.
	o := reviewIngestOutcome{
		Artifact:    "x.md",
		Disposition: "refused",
		Reason:      "front matter must be the leading block",
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, absent := range []string{"verdict", "reviewer_family", "builder_family", "enqueued", "sha", "branch"} {
		if strings.Contains(body, `"`+absent+`"`) {
			t.Errorf("%q must be ABSENT on a refusal, not emitted empty: %s", absent, body)
		}
	}
	// The fields it CAN attest must be present.
	for _, present := range []string{"artifact", "disposition", "reason"} {
		if !strings.Contains(body, `"`+present+`"`) {
			t.Errorf("%q must be present: %s", present, body)
		}
	}
}

// enqueued=false and enqueued-unknown are different facts, so the field is a
// pointer. A plain bool would render "unknown" as "false" and a consumer would
// conclude nothing was queued.
func TestEnqueuedFalseIsDistinctFromUnknown(t *testing.T) {
	known, err := json.Marshal(reviewIngestOutcome{Disposition: "duplicate", Enqueued: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(known), `"enqueued":false`) {
		t.Errorf("a known false must be emitted: %s", known)
	}
	unknown, err := json.Marshal(reviewIngestOutcome{Disposition: "refused"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unknown), "enqueued") {
		t.Errorf("an unknown enqueue state must be absent: %s", unknown)
	}
}

// A warning must be distinguishable from a failure, or a consumer cannot tell
// "needs attention" from "blocked".
func TestWarningIsDistinctFromFailure(t *testing.T) {
	rec := &preflightRecorder{asJSON: true}
	rec.pass("ok-check", "fine")
	rec.warn("fence-broker", "no fence broker")
	if rec.failed {
		t.Error("a warning must not fail the run")
	}
	if len(rec.checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(rec.checks))
	}
	w := rec.checks[1]
	if !w.Warning || w.Passed {
		t.Errorf("a warning must be marked warning and not passed: %+v", w)
	}
}

// In JSON mode a failing check must not exit early: the document has to be
// complete, and the exit status is applied when it is closed.
func TestJSONModeCollectsAllChecksBeforeExiting(t *testing.T) {
	rec := &preflightRecorder{asJSON: true}
	rec.fail("first", errTest{"boom"})
	rec.pass("second", "still recorded")
	if !rec.failed {
		t.Error("a failure must be recorded")
	}
	if len(rec.checks) != 2 {
		t.Fatalf("a failure must not truncate the document, got %d checks", len(rec.checks))
	}
	if rec.checks[1].Name != "second" {
		t.Error("checks after a failure must still be recorded in JSON mode")
	}
}

type errTest struct{ s string }

func (e errTest) Error() string { return e.s }

// The flag is matched the way preflight parses its arguments: bare positionals,
// not a FlagSet.
func TestPreflightJSONFlagIsRecognized(t *testing.T) {
	if !hasPreflightJSONFlag([]string{"--json"}) || !hasPreflightJSONFlag([]string{"-json"}) {
		t.Error("--json and -json must both be recognized")
	}
	if !hasPreflightJSONFlag([]string{"--full-tree", "--json"}) {
		t.Error("--json must be recognized alongside other flags")
	}
	if hasPreflightJSONFlag([]string{"--full-tree"}) {
		t.Error("absent --json must not be inferred")
	}
}
