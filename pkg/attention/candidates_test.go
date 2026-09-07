package attention

import (
	"encoding/json"
	"strings"
	"testing"
)

func readyCandidateFixture() CandidateObservation {
	return CandidateObservation{
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Task: "FAC-598", TaskStatus: "in-review", ReviewReady: true,
		RequiredChecks: []string{"Build"},
		PullRequests:   []CandidatePR{{Number: 42, HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "OPEN", Mergeable: "MERGEABLE", URL: "https://example.test/pr/42", Checks: []CandidateCheck{{Name: "Build", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://example.test/check/1"}}}},
	}
}

func TestCandidateAttentionEvidence(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*CandidateObservation)
		status  string
		visible bool
	}{
		{"open exact candidate", func(*CandidateObservation) {}, "ready-but-open", true},
		{"no PR", func(o *CandidateObservation) { o.PullRequests = nil }, "ready-without-pr", true},
		{"branch PR is a different SHA", func(o *CandidateObservation) { o.PullRequests[0].HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }, "ready-without-pr", true},
		{"provider timeout is unknown", func(o *CandidateObservation) { o.PullRequests = nil; o.EvidenceError = "provider_timeout" }, "ready-evidence-unknown", true},
		{"missing required check", func(o *CandidateObservation) { o.PullRequests[0].Checks = nil }, "ready-evidence-unknown", true},
		{"failed check", func(o *CandidateObservation) { o.PullRequests[0].Checks[0].Conclusion = "FAILURE" }, "ready-but-ci-failed", true},
		{"skipped check is not green", func(o *CandidateObservation) { o.PullRequests[0].Checks[0].Conclusion = "SKIPPED" }, "ready-but-ci-failed", true},
		{"pending check", func(o *CandidateObservation) { o.PullRequests[0].Checks[0].Status = "IN_PROGRESS" }, "ready-but-ci-pending", true},
		{"duplicate check", func(o *CandidateObservation) {
			o.PullRequests[0].Checks = append(o.PullRequests[0].Checks, o.PullRequests[0].Checks[0])
		}, "ready-evidence-unknown", true},
		{"conflict", func(o *CandidateObservation) { o.PullRequests[0].Mergeable = "CONFLICTING" }, "ready-but-conflicting", true},
		{"unknown mergeability", func(o *CandidateObservation) { o.PullRequests[0].Mergeable = "UNKNOWN" }, "ready-evidence-unknown", true},
		{"callback veto", func(o *CandidateObservation) { o.CallbackBlocked = true }, "ready-callback-blocked", true},
		{"missing task", func(o *CandidateObservation) { o.TaskStatus = "" }, "ready-evidence-unknown", true},
		{"consumed", func(o *CandidateObservation) { o.Consumed = true }, "", false},
		{"superseded", func(o *CandidateObservation) { o.Superseded = true }, "", false},
		{"task done", func(o *CandidateObservation) { o.TaskStatus = "done" }, "", false},
		{"not review ready", func(o *CandidateObservation) { o.ReviewReady = false }, "", false},
		{"already merged", func(o *CandidateObservation) { o.PullRequests[0].State = "MERGED" }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := readyCandidateFixture()
			tc.change(&o)
			got, visible := ClassifyCandidate(o)
			if visible != tc.visible || visible && got.Status != tc.status {
				t.Fatalf("got visible=%v status=%q; want %v %q", visible, got.Status, tc.visible, tc.status)
			}
			if got.MergeReady != nil && *got.MergeReady {
				t.Fatal("attention granted merge authority")
			}
			if visible && got.Level != LevelCritical {
				t.Fatalf("level = %v", got.Level)
			}
			if visible && got.PullRequest != 0 && got.URL == "" {
				t.Fatal("lost PR evidence link")
			}
		})
	}
}

func TestCandidateEscalationUsesExactIdentity(t *testing.T) {
	item, _ := ClassifyCandidate(readyCandidateFixture())
	first, key := EscalateCandidate(item, nil)
	if first.Beats != 1 || first.Escalated {
		t.Fatalf("first beat: %+v", first)
	}
	previous := map[string]int{key: first.Beats}
	second, _ := EscalateCandidate(item, previous)
	if second.Beats != 2 || !second.Escalated {
		t.Fatalf("second beat: %+v", second)
	}
	item.Status = "ready-but-ci-pending"
	changed, _ := EscalateCandidate(item, previous)
	if changed.Beats != 2 || !changed.Escalated {
		t.Fatal("check-state change hid a candidate that stayed ready")
	}
	item.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fresh, _ := EscalateCandidate(item, previous)
	if fresh.Beats != 1 || fresh.Escalated {
		t.Fatal("new SHA inherited stale escalation")
	}
}

func TestCandidateResultDoesNotCountPRAsLane(t *testing.T) {
	item, _ := ClassifyCandidate(readyCandidateFixture())
	for _, total := range []int{0, 2} {
		r := Result{Total: total, Candidates: []CandidateItem{item}}
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Total   int    `json:"total"`
			Needing int    `json:"needing"`
			State   string `json:"state"`
			Scanned bool   `json:"scanned"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != total || result.Needing != 0 || result.Scanned != (total > 0) {
			t.Fatalf("PR changed roster counts: %s", body)
		}
		expected := "ATTENTION"
		if total == 0 {
			expected = "UNKNOWN"
		}
		if result.State != expected {
			t.Fatalf("state = %s; want %s", result.State, expected)
		}
		if summary := Summary(r); strings.Contains(summary, "fleet healthy") || !strings.Contains(summary, "1 candidate(s)") {
			t.Fatalf("misleading summary: %s", summary)
		}
	}
	r := Result{Total: 2, CandidateError: "remote snapshot timed out"}
	if attentionState(r) != "UNKNOWN" || strings.Contains(Summary(r), "fleet healthy") {
		t.Fatalf("provider failure reported healthy: %s", Summary(r))
	}
}
