package attention

import (
	"fmt"
	"sort"
	"strings"
)

// CandidateObservation contains read-only evidence for one exact ready-set
// entry. The caller must join the canonical ledger, task, callback and remote
// observations for this SHA; an index entry alone cannot populate ReviewReady.
type CandidateObservation struct {
	LandingReceipt                string
	SHA, Branch, Task, TaskStatus string
	ReviewReady                   bool
	Consumed, Superseded          bool
	CallbackBlocked               bool
	EvidenceError                 string
	PullRequests                  []CandidatePR
	RequiredChecks                []string
}

type CandidatePR struct {
	Number                         int
	HeadSHA, State, Mergeable, URL string
	Checks                         []CandidateCheck
}

type CandidateCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

// CandidateItem is distinct from a lane: a PR must never count as a scanned
// standing agent. MergeReady is false for observed blockers, and null when the
// normal integration admission has not run. Attention never grants consent.
type CandidateItem struct {
	SHA            string           `json:"sha"`
	Branch         string           `json:"branch,omitempty"`
	Task           string           `json:"task"`
	PullRequest    int              `json:"pull_request,omitempty"`
	URL            string           `json:"url,omitempty"`
	Level          AttentionLevel   `json:"level"`
	Status         string           `json:"status"`
	Reason         string           `json:"reason"`
	ReviewReady    bool             `json:"review_ready"`
	LandingReceipt string           `json:"landing_receipt,omitempty"`
	MergeReady     *bool            `json:"merge_ready"`
	Checks         []CandidateCheck `json:"required_checks,omitempty"`
	Beats          int              `json:"beats"`
	Escalated      bool             `json:"escalated"`
}

// ClassifyCandidate does not mutate evidence or perform merge admission. A
// failed/unknown provider read is not evidence that no PR exists.
func ClassifyCandidate(o CandidateObservation) (CandidateItem, bool) {
	item := CandidateItem{SHA: o.SHA, Branch: o.Branch, Task: o.Task, Level: LevelCritical, ReviewReady: o.ReviewReady}
	if !o.ReviewReady || o.Consumed || o.Superseded {
		return item, false
	}
	blocked := false
	item.MergeReady = &blocked
	if o.EvidenceError != "" {
		item.Status, item.Reason = "ready-evidence-unknown", o.EvidenceError
		return item, true
	}
	switch strings.ToLower(o.TaskStatus) {
	case "done", "archived":
		return item, false
	case "in-review", "in-progress", "to-do":
	case "":
		item.Status, item.Reason = "ready-evidence-unknown", "current task status was not observed"
		return item, true
	default:
		item.Status, item.Reason = "ready-task-held", "task is not in an actionable state: "+o.TaskStatus
		return item, true
	}
	if o.LandingReceipt != "" {
		item.LandingReceipt = o.LandingReceipt
		item.Status, item.Reason = "ready-landing-recorded", "an exact-candidate landing receipt exists; validate completion before closure, do not harvest again"
		return item, true
	}
	if o.CallbackBlocked {
		item.Status, item.Reason = "ready-callback-blocked", "current exact-candidate callback blocks integration despite historical review readiness"
		return item, true
	}
	var exact []CandidatePR
	for _, pr := range o.PullRequests {
		if pr.HeadSHA == o.SHA {
			exact = append(exact, pr)
		}
	}
	if len(exact) == 0 {
		item.Status, item.Reason = "ready-without-pr", "no observed PR identity matches the exact reviewed SHA"
		return item, true
	}
	if len(exact) != 1 || exact[0].Number <= 0 {
		item.Status, item.Reason = "ready-evidence-unknown", "remote PR identity is ambiguous or malformed"
		return item, true
	}
	pr := exact[0]
	item.PullRequest, item.URL = pr.Number, pr.URL
	if strings.EqualFold(pr.State, "MERGED") {
		return item, false
	}
	if !strings.EqualFold(pr.State, "OPEN") {
		item.Status, item.Reason = "ready-pr-closed", "exact reviewed candidate has no open PR; inspect its closed disposition"
		return item, true
	}
	if len(o.RequiredChecks) == 0 {
		item.Status, item.Reason = "ready-evidence-unknown", "required-check policy was not resolved"
		return item, true
	}
	unknown, pending, failed := false, false, false
	for _, name := range o.RequiredChecks {
		var matches []CandidateCheck
		for _, check := range pr.Checks {
			if strings.EqualFold(strings.TrimSpace(check.Name), strings.TrimSpace(name)) {
				matches = append(matches, check)
			}
		}
		if len(matches) != 1 || strings.TrimSpace(name) == "" {
			unknown = true
			item.Checks = append(item.Checks, CandidateCheck{Name: name, Status: "UNKNOWN"})
			continue
		}
		c := matches[0]
		item.Checks = append(item.Checks, c)
		switch strings.ToUpper(c.Status) {
		case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED":
			pending = true
		case "COMPLETED":
			switch strings.ToUpper(c.Conclusion) {
			case "SUCCESS", "NEUTRAL":
			case "SKIPPED", "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
				failed = true
			default:
				unknown = true
			}
		default:
			unknown = true
		}
	}
	sort.Slice(item.Checks, func(i, j int) bool { return item.Checks[i].Name < item.Checks[j].Name })
	switch {
	case failed:
		item.Status, item.Reason = "ready-but-ci-failed", "review is ready but a required check failed"
	case unknown:
		item.Status, item.Reason = "ready-evidence-unknown", "required checks are missing, ambiguous or unrecognized"
	case pending:
		item.Status, item.Reason = "ready-but-ci-pending", "review is ready but a required check is still pending"
	case strings.EqualFold(pr.Mergeable, "CONFLICTING"):
		item.Status, item.Reason = "ready-but-conflicting", "review is ready but the live PR has merge conflicts"
	case !strings.EqualFold(pr.Mergeable, "MERGEABLE"):
		item.Status, item.Reason = "ready-evidence-unknown", "live PR mergeability is unknown"
	default:
		item.MergeReady = nil
		item.Status, item.Reason = "ready-but-open", "review and required checks are ready; coordinator must run normal integration admission for the open PR"
	}
	return item, true
}

// EscalateCandidate is keyed by exact SHA and PR, never by a moving
// branch name. The caller persists the counters only after the beat completes.
func EscalateCandidate(item CandidateItem, previous map[string]int) (CandidateItem, string) {
	key := fmt.Sprintf("%s:%d", item.SHA, item.PullRequest)
	item.Beats = previous[key] + 1
	item.Escalated = item.Beats > 1
	if item.Escalated {
		item.Reason = fmt.Sprintf("ESCALATED after %d beats: %s", item.Beats, item.Reason)
	}
	return item, key
}
