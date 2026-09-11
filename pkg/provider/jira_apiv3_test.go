package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jiraADFIssue is what Jira Cloud REST v3 actually returns: description is an
// Atlassian Document Format object, not a string.
const jiraADFIssue = `{
  "id": "10042", "key": "HUB-7",
  "fields": {
    "summary": "Migrate hub board",
    "description": {"type":"doc","version":1,"content":[
      {"type":"paragraph","content":[{"type":"text","text":"First line."}]},
      {"type":"paragraph","content":[{"type":"text","text":"Second line."}]}]},
    "status": {"name": "In Progress"},
    "priority": {"name": "High"},
    "labels": ["migration"],
    "created": "2026-09-01T12:00:00.000+0000",
    "project": {"key": "HUB"}
  }
}`

func jiraTestProvider(t *testing.T, h http.HandlerFunc) *JiraProvider {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	j := NewJiraProvider(ts.URL, "op@example.com", "token")
	j.ProjectKey = "HUB"
	return j
}

// Jira v3 returns ADF for description. Decoding it into a Go string fails, so
// every issue that HAS a description is unreadable.
func TestJiraDecodesADFDescription(t *testing.T) {
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jiraADFIssue))
	})
	task, err := j.GetTask(t.Context(), "HUB-7")
	if err != nil {
		t.Fatalf("a normal Jira v3 issue with a description failed to decode: %v", err)
	}
	if !strings.Contains(task.Description, "First line.") || !strings.Contains(task.Description, "Second line.") {
		t.Fatalf("ADF description was not flattened to text: %q", task.Description)
	}
}

// Jira's created timestamp uses a numeric offset with no colon, which is not
// RFC3339 and so does not decode into time.Time.
func TestJiraDecodesJiraTimestampFormat(t *testing.T) {
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jiraADFIssue))
	})
	task, err := j.GetTask(t.Context(), "HUB-7")
	if err != nil {
		t.Fatalf("jira timestamp format failed to decode: %v", err)
	}
	if task.CreatedAt.IsZero() {
		t.Fatal("created timestamp was dropped")
	}
}

// A board with more issues than one page must not silently return one page.
func TestJiraListTasksPaginatesInsteadOfSilentlyTruncating(t *testing.T) {
	var calls int
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		startAt := r.URL.Query().Get("startAt")
		issue := func(n int) string {
			return fmt.Sprintf(`{"id":"%d","key":"HUB-%d","fields":{"summary":"s","status":{"name":"To Do"},"priority":{"name":"Medium"},"created":"2026-09-01T12:00:00.000+0000","project":{"key":"HUB"}}}`, n, n)
		}
		if startAt == "" || startAt == "0" {
			fmt.Fprintf(w, `{"startAt":0,"maxResults":2,"total":3,"issues":[%s,%s]}`, issue(1), issue(2))
			return
		}
		fmt.Fprintf(w, `{"startAt":2,"maxResults":2,"total":3,"issues":[%s]}`, issue(3))
	})
	tasks, err := j.ListTasks(t.Context(), "HUB", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("silent truncation: got %d of 3 issues after %d call(s)", len(tasks), calls)
	}
}

// Jira transitions are applied by ID. Posting a status NAME is silently wrong:
// the API rejects it, and a fleet that believes the board moved is worse than
// one that knows it did not.
func TestJiraUpdateStatusResolvesTransitionID(t *testing.T) {
	var posted map[string]any
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodGet:
			w.Write([]byte(`{"transitions":[{"id":"31","name":"Done","to":{"name":"Done"}},{"id":"21","name":"In Progress","to":{"name":"In Progress"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Write([]byte(`{"id":"10042","key":"HUB-7","fields":{"summary":"s","status":{"name":"Done"},"priority":{"name":"High"},"created":"2026-09-01T12:00:00.000+0000","project":{"key":"HUB"}}}`))
		}
	})
	if err := j.UpdateStatus(t.Context(), "HUB-7", "done"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	tr, _ := posted["transition"].(map[string]any)
	if tr == nil || tr["id"] != "31" {
		t.Fatalf("transition was not applied by resolved id: %+v", posted)
	}
}

// A status with no matching transition must refuse, not post a guess.
func TestJiraUpdateStatusRefusesUnavailableTransition(t *testing.T) {
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodGet {
			w.Write([]byte(`{"transitions":[{"id":"21","name":"In Progress","to":{"name":"In Progress"}}]}`))
			return
		}
		if r.Method == http.MethodPost {
			t.Error("posted a transition that the board never offered")
		}
		w.Write([]byte(`{"id":"1","key":"HUB-7","fields":{"summary":"s","status":{"name":"To Do"},"priority":{"name":"Low"},"created":"2026-09-01T12:00:00.000+0000","project":{"key":"HUB"}}}`))
	})
	if err := j.UpdateStatus(t.Context(), "HUB-7", "done"); err == nil {
		t.Fatal("UpdateStatus accepted a status the board offers no transition for")
	}
}

// JQL is a query language: a project key must be bound, not interpolated.
func TestJiraRefusesUnsafeProjectKey(t *testing.T) {
	j := jiraTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a malformed project key reached the network: %s", r.URL.RawQuery)
		w.Write([]byte(`{"issues":[]}`))
	})
	if _, err := j.ListTasks(t.Context(), "HUB' OR project != 'x", ""); err == nil {
		t.Fatal("ListTasks accepted a project key containing JQL syntax")
	}
}
