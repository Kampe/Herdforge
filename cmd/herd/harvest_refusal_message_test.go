package main

import (
	"os/exec"
	"strings"
	"testing"
)

// FAC-685. Reported live: `herd review-ledger readiness` reported ready=true for
// CHA-2265 40b9006a (1 PASS, no dissent, provenance_unrecorded=0) while harvest
// refused with "last_pass_sha empty". Both were right. There WAS a PASS; no ref
// reached that commit, so it was not selectable for the branch being harvested.
// Harvest simply never said which question it had answered, and told the
// operator to pass `--candidate <last_pass_sha>` when that value was empty --
// a remedy that cannot be followed.

func TestReadinessRejectsAnUnknownFlagInsteadOfScoringItAsACandidate(t *testing.T) {
	// `readiness --sha <x>` used to emit
	//   {"sha":"--sha","ready":false,"reason":"no verdict recorded"}
	// next to the real answer. A caller reading the first element saw
	// not-ready for a candidate that does not exist: a mistyped flag
	// manufacturing a false verdict.
	binary := buildHerd(t)
	out, err := exec.Command(binary, "review-ledger", "readiness", "--sha", strings.Repeat("a", 40)).CombinedOutput()
	if err == nil {
		t.Fatalf("an unknown flag was accepted: %s", out)
	}
	text := string(out)
	if !strings.Contains(text, "unknown flag") {
		t.Fatalf("refusal does not name the cause: %s", text)
	}
	if strings.Contains(text, `"sha":"--sha"`) {
		t.Fatalf("the flag was still scored as a candidate: %s", text)
	}
}
