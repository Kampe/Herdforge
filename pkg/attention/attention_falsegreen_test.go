package attention

import "testing"

// An empty roster means nothing was scanned. Reporting that as "fleet healthy"
// is a false green: it is exactly the state a broken roster lookup produces, and
// it hid a done orchestrator and an idle review supervisor from triage.
func TestSummary_EmptyRosterIsNotHealthy(t *testing.T) {
	got := Summary(Result{Total: 0, Needing: 0})
	if got == "" {
		t.Fatal("empty summary")
	}
	for _, bad := range []string{"fleet healthy", "none need eyes"} {
		if contains(got, bad) {
			t.Fatalf("zero-lane scan must not claim health, got %q", got)
		}
	}
	if !contains(got, "UNKNOWN") {
		t.Fatalf("zero-lane scan must report UNKNOWN, got %q", got)
	}
}

// A real scan with nothing wrong must still be reportable as healthy, so the
// guard above cannot be satisfied by simply never saying "healthy".
func TestSummary_ScannedAndCleanIsStillHealthy(t *testing.T) {
	got := Summary(Result{Total: 14, Needing: 0})
	if !contains(got, "fleet healthy") {
		t.Fatalf("a real clean scan must report healthy, got %q", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
