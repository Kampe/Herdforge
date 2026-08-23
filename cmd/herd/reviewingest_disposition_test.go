package main

import "testing"

// FAC-581: a re-ingested verdict row (enqueued == false) is a duplicate no
// matter which way the verdict went. The pre-fix rule read
// `!enqueued && a.Verdict == "PASS"`, so replaying the historical review inbox
// reported every stale FAIL as freshly ADMITTED, re-applied days-old FAIL
// transitions, and reverted ~443 current cards to to-do.
func TestIngestDispositionIgnoresVerdictPolarity(t *testing.T) {
	verdicts := []string{"PASS", "FAIL", "BLOCKED", "superseded", ""}

	for _, v := range verdicts {
		if got := ingestDisposition(false, v); got != dispositionDuplicate {
			t.Errorf("verdict %q: an existing row must be a duplicate: got %q want %q",
				v, got, dispositionDuplicate)
		}
		if got := ingestDisposition(true, v); got != dispositionAdmitted {
			t.Errorf("verdict %q: a newly enqueued row must be admitted: got %q want %q",
				v, got, dispositionAdmitted)
		}
	}
}
