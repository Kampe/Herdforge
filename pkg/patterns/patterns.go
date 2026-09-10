// Package patterns holds shared regex patterns and message constants used
// across harvest, process, and attention classification logic.
//
// FAC-790: six regex patterns and one message constant were originally scattered
// across pkg/harvest/classify.go, pkg/process/process.go, and
// pkg/attention/attention.go, causing copies to diverge. This package ensures
// one definition is shared everywhere.
//
// The review marker patterns exclude text that merely discusses review topics
// (verdict/findings/confirmed) from being classified as actual quota failures or
// output limit events. The output limit message is a canonical constant that
// appears in multiple classification decision points.
package patterns

import (
	"regexp"
	"sync"
)

// ReviewMarkerPattern is a regex that matches text indicating a code review
// context. Used to exclude review prose from quota/output-limit classifications.
//
// Matches: verdict, merge recommendation, confirmed, findings/finding, reviewing,
// pass/fail decision markers.
//
// Rationale: A code reviewer discussing a rate-limit bug or output truncation
// should not poison the actual execution classification. The pattern excludes
// review metadata while allowing the text to be classified by other signals.
var (
	reviewMarkerOnce sync.Once
	reviewMarkerRe   *regexp.Regexp
)

func ReviewMarkerPattern() *regexp.Regexp {
	reviewMarkerOnce.Do(func() {
		reviewMarkerRe = regexp.MustCompile(`(?i)verdict:\s*|merge recommendation:\s*|\bconfirmed\b|\bfindings?\b|reviewing|pass/fail`)
	})
	return reviewMarkerRe
}

// OutputLimitMessage is the canonical message emitted when a pane's output
// was truncated due to an output token/context length limit (e.g. finish=length).
//
// Used in process.OutputLimitReason() to report the reason, and checked in
// attention.Triage() to classify the lane's terminal state.
const OutputLimitMessage = "output token limit reached (finish=length)"

// OutputLimitPattern is a regex that matches various phrasings of output/token
// limit exhaustion across different providers and APIs.
//
// Matches variations like:
//   - finish=length, finish_reason=length
//   - output token limit reached / output limit exceeded
//   - maximum context/token/output length reached
//   - max tokens reached, response truncated due to output limit
var (
	outputLimitOnce sync.Once
	outputLimitRe   *regexp.Regexp
)

func OutputLimitPattern() *regexp.Regexp {
	outputLimitOnce.Do(func() {
		outputLimitRe = regexp.MustCompile(
			`(?i)finish[=_]reason[:=]\s*["']?length["']?|` +
				`finish=length|` +
				`output token limit reached|` +
				`output limit (exceeded|reached|hit)|` +
				`maximum (context|token|output) length (exceeded|reached)|` +
				`max(?:imum)? tokens reached|` +
				`response truncated due to output limit`,
		)
	})
	return outputLimitRe
}
