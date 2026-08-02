package throughput

import (
	"testing"
	"time"
)

func w(t *testing.T, start, end string) Window {
	t.Helper()
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		t.Fatal(err)
	}
	e, err := time.Parse(time.RFC3339, end)
	if err != nil {
		t.Fatal(err)
	}
	return Window{Start: s, End: e}
}

func TestMedianLatency(t *testing.T) {
	if MedianLatency(nil) != -1 {
		t.Error("empty must be -1")
	}
	if MedianLatency([]int{5, 1, 9}) != 5 {
		t.Error("odd must be middle")
	}
	if MedianLatency([]int{4, 10}) != 7 {
		t.Error("even must be (a+b)/2")
	}
	if MedianLatency([]int{3, 4}) != 3 {
		t.Error("even truncates: (3+4)/2 = 3")
	}
}

func TestIsoEpoch(t *testing.T) {
	if IsoEpoch("2026-08-01T00:00:00Z") == 0 {
		t.Error("Z form must parse")
	}
	if IsoEpoch("2026-08-01T00:00:00+0200") == 0 {
		t.Error("%z form must parse")
	}
	if IsoEpoch("garbage") != 0 {
		t.Error("junk must be 0")
	}
}

func TestCountRouteLines(t *testing.T) {
	lines := []string{
		"2026-08-01T10:00:00Z route claude",
		"2026-08-01T11:00:00Z route codex",
		"2026-07-01T10:00:00Z route old",
		"no timestamp here",
		"",
	}
	got := CountRouteLines(lines, "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	if got != 2 {
		t.Errorf("want 2 in-window T lines, got %d", got)
	}
}

func TestComputeCore(t *testing.T) {
	win := w(t, "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z") // 86400s
	commits := []CommitLine{
		{SHA: "aaa", Stamp: "2026-08-01T10:00:00Z", Subject: "feat: land thing (FAC-477)"},
		{SHA: "bbb", Stamp: "2026-08-01T12:00:00Z", Subject: "fix: follow-up FAC-477 and CHA-9"},
	}
	verdicts := []VerdictLine{
		{SHA: "aaa", Stamp: "2026-08-01T09:00:00Z", Verdict: "PASS"},   // matched: 3600s
		{SHA: "ghost", Stamp: "2026-08-01T09:30:00Z", Verdict: "PASS"}, // rebased pin: excluded
		{SHA: "aaa", Stamp: "2026-08-01T08:00:00Z", Verdict: "FAIL"},   // round only
		{SHA: "bbb", Stamp: "2026-08-01T13:00:00Z", Verdict: "PASS"},   // verdict AFTER merge: excluded
	}
	m := Compute(commits, verdicts, 4, win)

	if m.MergedCommits != 2 || m.MergesPerDay != 2.00 {
		t.Errorf("merged: %d %v", m.MergedCommits, m.MergesPerDay)
	}
	if m.MergedTickets != 2 { // FAC-477 deduped, CHA-9 counted
		t.Errorf("merged_tickets = %d, want 2", m.MergedTickets)
	}
	if m.ReviewRounds != 4 || m.ReviewedTickets != 3 || m.PassVerdicts != 3 {
		t.Errorf("rounds=%d reviewed=%d pass=%d", m.ReviewRounds, m.ReviewedTickets, m.PassVerdicts)
	}
	if m.MatchedVerdictLatencies != 1 || m.MedianVerdictToMergeSeconds != 3600 {
		t.Errorf("latency: n=%d median=%d", m.MatchedVerdictLatencies, m.MedianVerdictToMergeSeconds)
	}
	if m.LatencyNote != "exact SHA matches only" {
		t.Errorf("latency_note = %q", m.LatencyNote)
	}
	if m.ReviewRoundsPerTicket != 1.33 {
		t.Errorf("rounds/ticket = %v, want 1.33", m.ReviewRoundsPerTicket)
	}
	if m.RouteDecisionsPerMergedTicket != 2.00 {
		t.Errorf("route/ticket = %v, want 2.00", m.RouteDecisionsPerMergedTicket)
	}
}

func TestComputeEmptyEvidence(t *testing.T) {
	win := w(t, "2026-08-01T00:00:00Z", "2026-08-02T00:00:00Z")
	m := Compute(nil, nil, 0, win)
	if m.MedianVerdictToMergeSeconds != -1 {
		t.Error("no latencies must be median -1")
	}
	if m.LatencyNote != "no exact SHA verdict-to-merge matches in window" {
		t.Errorf("latency_note = %q", m.LatencyNote)
	}
	if m.RouteDecisionsPerMergedTicket != -1 {
		t.Error("no merged tickets must be -1")
	}
	if m.ReviewRoundsPerTicket != 0 {
		t.Error("no reviewed tickets must be 0.00")
	}
}
