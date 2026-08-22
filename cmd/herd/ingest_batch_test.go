package main

import (
	"os"
	"strings"
	"testing"
)

// TestOneBadArtifactDoesNotAbortTheBatch is the FAC-580 gate.
//
// Three paths called os.Exit(1) on a per-artifact failure, so a single
// malformed artifact aborted the whole run and nothing after it was ingested.
// One file declaring an unprovable builder family stalled 99 good verdicts
// behind it — invisible from outside, because the command reports a failure for
// one name and silently never reaches the rest.
//
// Structural, because the loop needs a live ledger: assert no per-artifact
// failure path exits the process.
func TestOneBadArtifactDoesNotAbortTheBatch(t *testing.T) {
	src, err := os.ReadFile("reviewingest.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runReviewIngest")
	if !ok {
		t.Fatal("cannot locate runReviewIngest")
	}
	// The per-artifact loop begins at the file iteration.
	loopStart := strings.Index(body, "for _, f := range files {")
	if loopStart < 0 {
		t.Fatal("cannot locate the per-artifact loop")
	}
	loop := body[loopStart:]
	// Slice at the first POST-loop statement. The summary encode and the
	// refusal exit both live after the loop and are legitimate; only exits
	// INSIDE the per-artifact loop truncate a batch.
	end := strings.Index(loop, "if err := emit.summary(")
	if end < 0 {
		t.Fatal("cannot locate the end of the per-artifact loop")
	}
	loop = loop[:end]
	if strings.Contains(loop, "os.Exit(") {
		t.Error("a per-artifact failure must refuse that artifact and continue; " +
			"exiting inside the loop silently truncates the batch")
	}
	// And the batch must still fail closed overall.
	if !strings.Contains(body, "if refused > 0 {") {
		t.Error("any refusal must still produce a non-zero exit")
	}
}
