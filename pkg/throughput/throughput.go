// Package throughput ports bin/herd-throughput: read-only fleet throughput
// KPIs from durable local evidence (main-ref git log, the review verdict
// ledger, the route-decisions log). No SQLite, no provider, no git writes.
//
// The merge count is deliberately named merged_commits: this repository uses
// rebase merges, so a merge commit count is always zero. Exact verdict
// latency is reported only when a PASS sha is itself present in the selected
// main log; unmatched rebased pins are excluded rather than guessed.
package throughput

import (
	"regexp"
	"sort"
	"strconv"
	"time"
)

// CommitLine is one main-ref log row (git log --format=%H%x09%cI%x09%s).
type CommitLine struct {
	SHA     string
	Stamp   string // ISO commit time
	Subject string
}

// VerdictLine is one review-ledger verdict event row.
type VerdictLine struct {
	SHA     string
	Stamp   string
	Verdict string
}

// Window is the evidence time window.
type Window struct {
	Start time.Time
	End   time.Time
}

// Metric is the aggregate packet, keys mirroring the zsh jq object exactly.
type Metric struct {
	WindowStart                   string  `json:"window_start"`
	WindowEnd                     string  `json:"window_end"`
	MergedCommits                 int     `json:"merged_commits"`
	MergesPerDay                  float64 `json:"merges_per_day"`
	MergedTickets                 int     `json:"merged_tickets"`
	ReviewRounds                  int     `json:"review_rounds"`
	ReviewedTickets               int     `json:"reviewed_tickets"`
	ReviewRoundsPerTicket         float64 `json:"review_rounds_per_ticket"`
	PassVerdicts                  int     `json:"pass_verdicts"`
	MatchedVerdictLatencies       int     `json:"matched_verdict_latencies"`
	MedianVerdictToMergeSeconds   int     `json:"median_verdict_to_merge_seconds"`
	RouteDecisions                int     `json:"route_decisions"`
	RouteDecisionsPerMergedTicket float64 `json:"route_decisions_per_merged_ticket"`
	LatencyNote                   string  `json:"latency_note"`
	QuotaNote                     string  `json:"quota_note"`
}

// ticketToken generalizes the zsh's CHA-[0-9]+: this forge's refs are
// FAC-N, chainseer's are CHA-N — any uppercase PREFIX-N counts.
var ticketToken = regexp.MustCompile(`[A-Z]+-[0-9]+`)

const isoLayout = "2006-01-02T15:04:05Z"

// IsoEpoch parses BSD-first layouts (Z form, then %z offset form), 0 on
// total failure — mirroring iso_epoch.
func IsoEpoch(iso string) int64 {
	if t, err := time.Parse(isoLayout, iso); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse("2006-01-02T15:04:05-0700", iso); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.Unix()
	}
	return 0
}

// TwoDecimals reproduces awk printf %.2f.
func TwoDecimals(n float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(n, 'f', 2, 64), 64)
	return f
}

// MedianLatency is the zsh integer median: odd → middle, even → (a+b)/2
// truncated. Returns -1 for an empty list (median unknown, never guessed).
func MedianLatency(ns []int) int {
	if len(ns) == 0 {
		return -1
	}
	s := append([]int(nil), ns...)
	sort.Ints(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// CountRouteLines mirrors the awk filter: a line counts when it contains
// "T" and sorts lexicographically within [since, until] (ISO strings sort
// chronologically).
func CountRouteLines(lines []string, since, until string) int {
	n := 0
	for _, l := range lines {
		if len(l) == 0 {
			continue
		}
		if indexByte(l, 'T') < 0 {
			continue
		}
		if l >= since && l <= until {
			n++
		}
	}
	return n
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Compute aggregates the evidence into the Metric packet.
func Compute(commits []CommitLine, verdicts []VerdictLine, routeDecisions int, w Window) Metric {
	windowSec := w.End.Unix() - w.Start.Unix()
	if windowSec < 1 {
		windowSec = 1
	}

	m := Metric{
		WindowStart:    w.Start.UTC().Format(isoLayout),
		WindowEnd:      w.End.UTC().Format(isoLayout),
		MergedCommits:  len(commits),
		RouteDecisions: routeDecisions,
		QuotaNote:      "route-decision count is a local quota-spend proxy, not provider billing",
	}

	// merged_tickets: unique PREFIX-N tokens across the window sample.
	shaEpoch := map[string]int64{}
	ticketSeen := map[string]bool{}
	for _, c := range commits {
		shaEpoch[c.SHA] = IsoEpoch(c.Stamp)
		for _, tok := range ticketToken.FindAllString(c.Subject, -1) {
			ticketSeen[tok] = true
		}
	}
	m.MergedTickets = len(ticketSeen)

	// Verdict rounds, reviewed shas, PASS latency matching.
	reviewedSHA := map[string]bool{}
	var latencies []int
	for _, v := range verdicts {
		if v.Verdict == "" {
			continue
		}
		m.ReviewRounds++
		if !reviewedSHA[v.SHA] {
			reviewedSHA[v.SHA] = true
			m.ReviewedTickets++
		}
		if v.Verdict != "PASS" {
			continue
		}
		m.PassVerdicts++
		mergeEpoch := shaEpoch[v.SHA]
		verdictEpoch := IsoEpoch(v.Stamp)
		if mergeEpoch > 0 && verdictEpoch > 0 && mergeEpoch >= verdictEpoch {
			latencies = append(latencies, int(mergeEpoch-verdictEpoch))
		}
	}
	m.MatchedVerdictLatencies = len(latencies)
	m.MedianVerdictToMergeSeconds = MedianLatency(latencies)
	if m.MedianVerdictToMergeSeconds < 0 {
		m.LatencyNote = "no exact SHA verdict-to-merge matches in window"
	} else {
		m.LatencyNote = "exact SHA matches only"
	}

	m.MergesPerDay = TwoDecimals(float64(m.MergedCommits) * 86400 / float64(windowSec))
	if m.ReviewedTickets == 0 {
		m.ReviewRoundsPerTicket = 0
	} else {
		m.ReviewRoundsPerTicket = TwoDecimals(float64(m.ReviewRounds) / float64(m.ReviewedTickets))
	}
	if m.MergedTickets == 0 {
		m.RouteDecisionsPerMergedTicket = -1
	} else {
		m.RouteDecisionsPerMergedTicket = TwoDecimals(float64(m.RouteDecisions) / float64(m.MergedTickets))
	}
	return m
}
