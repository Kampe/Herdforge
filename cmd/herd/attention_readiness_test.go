package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/readyindex"
)

func TestAttentionCLIPathSurfacesExactReadyPRStates(t *testing.T) {
	root := t.TempDir()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	openSHA := strings.Repeat("a", 40)
	missingSHA := strings.Repeat("b", 40)
	staleSHA := strings.Repeat("d", 40)
	rows := `{"event":"verdict","sha":"` + openSHA + `","reviewer":"r1","verdict":"PASS","builder_family":"xai"}` + "\n" +
		`{"event":"verdict","sha":"` + missingSHA + `","reviewer":"r2","verdict":"PASS","builder_family":"google"}` + "\n" +
		`{"event":"verdict","sha":"` + staleSHA + `","reviewer":"r3","verdict":"PASS","builder_family":"anthropic"}` + "\n"
	if err := os.WriteFile(ledgerPath, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readyindex.Rebuild(readyindex.PathFor(ledgerPath), []readyindex.Entry{
		{SHA: openSHA, Branch: "herd/open"},
		{SHA: missingSHA, Branch: "herd/missing"},
		{SHA: staleSHA, Branch: "herd/stale"},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	lookup := func(branch string) ([]livePullRequest, error) {
		switch branch {
		case "herd/open":
			return []livePullRequest{{Number: 622, State: "OPEN", HeadRefOID: openSHA}}, nil
		case "herd/missing":
			return nil, nil
		case "herd/stale":
			// A PR for the same branch at another SHA is not the candidate's PR.
			return []livePullRequest{{Number: 621, State: "OPEN", HeadRefOID: strings.Repeat("c", 40)}}, nil
		default:
			t.Fatalf("unexpected branch lookup %q", branch)
			return nil, nil
		}
	}

	result := attention.Result{Total: 1, Counts: map[attention.AttentionLevel]int{attention.LevelNone: 1}}
	if err := appendReadyCandidateAttention(&result, ledgerPath, lookup); err != nil {
		t.Fatal(err)
	}
	if result.ReadyCandidates != 3 {
		t.Fatalf("ready candidate findings = %d, want 3", result.ReadyCandidates)
	}
	open := findCandidateItem(t, result.Items, openSHA)
	if open.Level != attention.LevelCritical || open.Status != "ready-but-open" {
		t.Fatalf("ready open PR was not critical and distinct: %+v", open)
	}
	if open.PullRequest != 622 || !strings.Contains(open.Reason, "PR #622") || !strings.Contains(open.Reason, openSHA) {
		t.Fatalf("ready open PR did not name its exact identity: %+v", open)
	}
	missing := findCandidateItem(t, result.Items, missingSHA)
	if missing.Level != attention.LevelCritical || missing.Status != "ready-without-pr" {
		t.Fatalf("ready candidate without an exact PR was not critical and distinct: %+v", missing)
	}
	if missing.PullRequest != 0 || !strings.Contains(missing.Reason, "unlandable") || !strings.Contains(missing.Reason, missingSHA) {
		t.Fatalf("ready candidate without PR did not name the unlandable exact SHA: %+v", missing)
	}
	stale := findCandidateItem(t, result.Items, staleSHA)
	if stale.Status != "ready-without-pr" || stale.PullRequest != 0 {
		t.Fatalf("a same-branch PR at another SHA must not satisfy exact identity: %+v", stale)
	}
	if open.Escalated || missing.Escalated || open.Beats != 1 || missing.Beats != 1 {
		t.Fatalf("first observation must be critical but not persisted escalation: open=%+v missing=%+v", open, missing)
	}

	// The same exact condition on the next attention beat must escalate instead
	// of printing the same quiet status forever.
	result = attention.Result{Total: 1, Counts: map[attention.AttentionLevel]int{attention.LevelNone: 1}}
	if err := appendReadyCandidateAttention(&result, ledgerPath, lookup); err != nil {
		t.Fatal(err)
	}
	open = findCandidateItem(t, result.Items, openSHA)
	missing = findCandidateItem(t, result.Items, missingSHA)
	stale = findCandidateItem(t, result.Items, staleSHA)
	if !open.Escalated || !missing.Escalated || !stale.Escalated || open.Beats != 2 || missing.Beats != 2 || stale.Beats != 2 {
		t.Fatalf("second consecutive beat must escalate: open=%+v missing=%+v stale=%+v", open, missing, stale)
	}
	if !strings.Contains(open.Reason, "ESCALATED") || !strings.Contains(missing.Reason, "ESCALATED") {
		t.Fatalf("escalation must be visible in human output: open=%q missing=%q", open.Reason, missing.Reason)
	}
}

func findCandidateItem(t *testing.T, items []attention.Item, sha string) attention.Item {
	t.Helper()
	for _, item := range items {
		if item.SHA == sha {
			return item
		}
	}
	t.Fatalf("candidate %s absent from attention items: %+v", sha, items)
	return attention.Item{}
}
