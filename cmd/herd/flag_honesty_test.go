package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNoFlagPromisesWhatItDiscards is the FAC-433 gate.
//
// The reported defect had two halves. One was a validator with no escape hatch,
// which lives in the consumer's own tooling. The other generalizes here: a
// `--lane` filter that was registered and then never applied, so it silently did
// nothing while its help text said otherwise.
//
// Herdforge had three of those. `--force` advertised "Bypass openusage cache"
// with no bypass implemented anywhere. `--loop` advertised running the
// autonomous loop and discarded the value, so `--loop=false` was ignored and an
// operator asking for a single pass got the loop. A flag that promises behaviour
// it does not deliver is worse than an absent flag, because the operator
// believes they changed something.
//
// A discarded flag is legitimate in exactly two cases, and both must be visible
// in the source: the help text says it is ignored, or a comment names the code
// that actually reads the value.
func TestNoFlagPromisesWhatItDiscards(t *testing.T) {
	discarded := regexp.MustCompile(`^_ = fs\.(String|Bool|Int|Float64)\("([^"]+)",[^,]+, "([^"]*)"\)`)
	for _, file := range []string{"main.go", "pulse.go", "review_pool.go", "reviewingest.go", "boarddone.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			m := discarded.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			name, help := m[2], m[3]
			if declaresIgnored(help) {
				continue
			}
			if precedingCommentExplains(lines, i) {
				continue
			}
			t.Errorf("%s:%d flag --%s is registered and discarded, but its help text (%q) promises behaviour. "+
				"Either implement it, say IGNORED in the help text, or add a comment naming the code that reads it.",
				file, i+1, name, help)
		}
	}
}

// declaresIgnored reports whether the help text tells the truth about doing
// nothing. Case-insensitive so IGNORED and ignored both count.
func declaresIgnored(help string) bool {
	l := strings.ToLower(help)
	return strings.Contains(l, "ignored") || strings.Contains(l, "deprecated") ||
		strings.Contains(l, "no-op")
}

// precedingCommentExplains reports whether a comment immediately above the
// registration names the real reader. "Registration-only" is the marker,
// because a bare comment could say anything.
func precedingCommentExplains(lines []string, idx int) bool {
	for i := idx - 1; i >= 0 && i >= idx-8; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			return false
		}
		if !strings.HasPrefix(t, "//") {
			return false
		}
		if strings.Contains(strings.ToLower(t), "registration-only") {
			return true
		}
	}
	return false
}

// TestLoopFalseRunsASinglePass pins the behaviour --loop=false always claimed.
func TestLoopFalseRunsASinglePass(t *testing.T) {
	if got := effectiveMaxTicks(false, 0); got != 1 {
		t.Errorf("--loop=false must run a single tick, got %d", got)
	}
	// An explicit count wins: a caller naming a number knows what they want.
	if got := effectiveMaxTicks(false, 5); got != 5 {
		t.Errorf("an explicit --ticks must be honoured over --loop=false, got %d", got)
	}
	// Looping keeps "run until drained".
	if got := effectiveMaxTicks(true, 0); got != 0 {
		t.Errorf("--loop=true must keep run-until-drained, got %d", got)
	}
	if got := effectiveMaxTicks(true, 3); got != 3 {
		t.Errorf("--ticks must survive --loop=true, got %d", got)
	}
}
