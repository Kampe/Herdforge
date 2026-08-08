package harvest

import (
	"regexp"
	"strings"
)

type Classification string

const (
	ClassificationNeedsReview Classification = "NEEDS_REVIEW"
	ClassificationPass        Classification = "PASS"
	ClassificationFail        Classification = "FAIL"
	ClassificationComplete    Classification = "COMPLETE"
	ClassificationBlocked     Classification = "BLOCKED"
	ClassificationQuota       Classification = "QUOTA"
	ClassificationUnconsumed  Classification = "UNCONSUMED"
	ClassificationUnknown     Classification = "UNKNOWN"
)

var (
	needsReviewRE = regexp.MustCompile(`(?i)NEEDS_REVIEW|Status:\s*NEEDS_REVIEW`)
	passRE        = regexp.MustCompile(`(?i)Merge recommendation:\s*YES|Verdict:\s*PASS`)
	failRE        = regexp.MustCompile(`(?i)Verdict:\s*FAIL|Merge recommendation:\s*NO`)
	completeRE    = regexp.MustCompile(`(?i)Status:\s*COMPLETE(?:\s|$)|COMPLETE\s*$`)
	blockedRE     = regexp.MustCompile(`(?i)Status:\s*BLOCKED|BLOCKED:`)
	// CHA-281: genuine provider exhaustion, not review content that discusses rate limits.
	// Requires exhaustion phrasing AND exclusion of text carrying review markers.
	quotaRE = regexp.MustCompile(`(?i)(out of credits|too many requests|429 too many|(rate\s*limit|usage limit|weekly limit|daily limit|monthly limit|token quota|api quota|quota)[^.]{0,24}(exceeded|reached|throttled|hit)|exceeded your (quota|rate|usage|limit))`)
	// Exclude review markers — a reviewer analyzing rate-limit code should not match QUOTA.
	reviewMarkerRE = regexp.MustCompile(`(?i)verdict:\s*|merge recommendation:\s*|\bconfirmed\b|\bfindings?\b|reviewing|pass/fail`)
	// Unconsumed: prompt prefix ❯ with no status/worked-for marker
	unconsumedRE   = regexp.MustCompile(`(?m)^❯\s`)
	statusWorkedRE = regexp.MustCompile(`(?i)Worked for|Status:`)
)

func ClassifyText(text string) Classification {
	if needsReviewRE.MatchString(text) {
		return ClassificationNeedsReview
	}
	if passRE.MatchString(text) {
		return ClassificationPass
	}
	if failRE.MatchString(text) {
		return ClassificationFail
	}
	if completeRE.MatchString(text) {
		return ClassificationComplete
	}
	if blockedRE.MatchString(text) {
		return ClassificationBlocked
	}
	// QUOTA: must match exhaustion pattern AND not match review markers.
	// This prevents false-positives when a reviewer discusses rate-limit/429/quota
	// code in a code review context.
	if quotaRE.MatchString(text) && !reviewMarkerRE.MatchString(text) {
		return ClassificationQuota
	}
	// Unconsumed: pane shows a prompt prefix but no status/work evidence.
	if unconsumedRE.MatchString(text) && !statusWorkedRE.MatchString(text) {
		return ClassificationUnconsumed
	}
	return ClassificationUnknown
}

type ProcessingItem struct {
	PaneID string         `json:"pane_id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Class  Classification `json:"class"`
	Action string         `json:"action"`
	Tail   string         `json:"tail"`
}

func ActionForClass(class Classification) string {
	switch class {
	case ClassificationNeedsReview:
		return "dispatch_review_or_merge_gate"
	case ClassificationPass:
		return "merge_if_tier_ok"
	case ClassificationFail:
		return "return_to_builder"
	case ClassificationComplete:
		return "close_or_activate"
	case ClassificationBlocked:
		return "unblock_or_reassign"
	case ClassificationQuota:
		return "mark_unavailable_and_reroute"
	case ClassificationUnconsumed:
		return "read_pane"
	default:
		return "read_pane"
	}
}

func Tail(text string, maxLen int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	tailLines := lines
	if len(tailLines) > 8 {
		tailLines = tailLines[len(tailLines)-8:]
	}
	joined := strings.Join(tailLines, " ")
	// Collapse whitespace
	spaces := regexp.MustCompile(`\s+`)
	joined = spaces.ReplaceAllString(joined, " ")
	if len(joined) > maxLen {
		joined = joined[:maxLen]
	}
	return joined
}
