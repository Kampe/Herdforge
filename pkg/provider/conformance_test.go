package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// FAC-124 conformance: error-body-in-200 rejected, pagination empty-page boundary,
// and mutation readback-drift detection across adapters.

func TestConformance_Kaneo_ErrorBodyIn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":"not authorized for project"}`))
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "proj-1", false)
	_, err := kp.GetTask(context.Background(), "task-1")
	if err == nil {
		t.Fatal("expected hard error on 200+error body, got nil")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.StatusCode != http.StatusOK {
		t.Errorf("StatusCode=%d want 200", pe.StatusCode)
	}
	if !strings.Contains(pe.Message, "not authorized") {
		t.Errorf("Message=%q", pe.Message)
	}

	// Non-vacuity: same adapter accepts a clean 200 body.
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"task-1","ref":"FAC-1","title":"ok","status":"to-do","priority":"low","projectId":"proj-1","labels":[]}`))
	}))
	defer okServer.Close()
	kp2 := NewKaneoProvider(okServer.URL, "proj-1", false)
	task, err := kp2.GetTask(context.Background(), "task-1")
	if err != nil || task == nil || task.ID != "task-1" {
		t.Fatalf("clean body must succeed: task=%+v err=%v", task, err)
	}
}

func TestConformance_Linear_GraphQLErrorsIn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"errors":[{"message":"Entity not found: Issue"}]}`))
	}))
	defer server.Close()

	lp := NewLinearProvider("key")
	lp.Client = server.Client()
	lp.BaseURL = server.URL

	_, err := lp.GetTask(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected GraphQL errors array under 200 to fail")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
}

func TestConformance_GitHub_ErrorBodyIn200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"Moved Permanently","error":"resource moved"}`))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("tok", "o", "r")
	gp.Client = &http.Client{Transport: &customTripper{targetURL: u}}

	_, err := gp.GetTask(context.Background(), "1")
	if err == nil {
		t.Fatal("expected error body under 200 to fail")
	}
}

func TestConformance_JiraAzure_ErrorBodyIn200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":"permission denied"}`))
	}))
	defer ts.Close()

	jp := NewJiraProvider(ts.URL, "u@e.com", "tok")
	_, err := jp.GetTask(context.Background(), "X-1")
	if err == nil {
		t.Fatal("jira: expected 200+error to fail")
	}

	ap := NewAzureDevOpsProvider(ts.URL+"/org", "proj", "pat")
	_, err = ap.GetTask(context.Background(), "1")
	if err == nil {
		t.Fatal("azure: expected 200+error to fail")
	}
}

func TestConformance_KaneoCLI_PaginationEmptyPageBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub test is POSIX-only")
	}

	// Page 1: short page of 3 (would wrongly terminate if short==end).
	// Page 2: 2 more.
	// Page 3: empty → stop. Total 5.
	dir := t.TempDir()
	stub := `#!/bin/sh
page=1
prev=""
for a in "$@"; do
  if [ "$prev" = "--page" ]; then page="$a"; fi
  prev="$a"
done
case "$page" in
  1) n=3; start=1 ;;
  2) n=2; start=4 ;;
  *) echo "[]"; exit 0 ;;
esac
printf "["
i=0
while [ $i -lt $n ]; do
  ref=$((start + i))
  [ $i -gt 0 ] && printf ","
  printf "{\"id\":\"id-%d\",\"ref\":\"FAC-%d\",\"title\":\"t\",\"status\":\"to-do\",\"priority\":\"low\",\"projectId\":\"p1\",\"labels\":[]}" "$ref" "$ref"
  i=$((i + 1))
done
printf "]"
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	tasks, err := kp.ListTasks(context.Background(), "p1", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 5 {
		t.Fatalf("short-page continuation failed: got %d want 5", len(tasks))
	}
	if tasks[4].Ref != "FAC-5" {
		t.Fatalf("last ref=%s want FAC-5", tasks[4].Ref)
	}

	// Non-vacuity: empty first page is legitimate empty list, not an error.
	emptyStub := `#!/bin/sh
echo "[]"
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(emptyStub), 0o755); err != nil {
		t.Fatal(err)
	}
	tasks, err = kp.ListTasks(context.Background(), "p1", "")
	if err != nil {
		t.Fatalf("empty page must not error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("empty page: got %d tasks", len(tasks))
	}
}

func TestConformance_GitHub_PaginationEmptyPage(t *testing.T) {
	pageHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageHits++
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch page {
		case "1":
			// Short page of 2 (per_page=100) — must continue.
			w.Write([]byte(`[
				{"number":1,"title":"a","body":"","state":"open","labels":[],"created_at":"2026-08-01T12:00:00Z"},
				{"number":2,"title":"b","body":"","state":"open","labels":[],"created_at":"2026-08-01T12:00:00Z"}
			]`))
		case "2":
			w.Write([]byte(`[
				{"number":3,"title":"c","body":"","state":"open","labels":[],"created_at":"2026-08-01T12:00:00Z"}
			]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("tok", "o", "r")
	gp.Client = &http.Client{Transport: &customTripper{targetURL: u}}

	tasks, err := gp.ListTasks(context.Background(), "", "open")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks want 3 (short page must not terminate)", len(tasks))
	}
	if pageHits < 3 {
		t.Fatalf("expected empty-page probe, pageHits=%d", pageHits)
	}
}

func TestConformance_Kaneo_ReadbackDrift(t *testing.T) {
	// Mutation reports success but readback status disagrees → hard error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Drift: still to-do after "done" write.
			w.Write([]byte(`{"id":"task-1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p","labels":[]}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	kp := NewKaneoProvider(server.URL, "p", false)
	err := kp.UpdateStatus(context.Background(), "task-1", "done")
	if err == nil {
		t.Fatal("expected readback drift error, got nil")
	}
	var re *ReadbackDriftError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ReadbackDriftError, got %T: %v", err, err)
	}
	if re.Expected != StatusDone || re.Actual != StatusToDo {
		t.Errorf("drift expected=%q actual=%q", re.Expected, re.Actual)
	}

	// Non-vacuity: matching readback succeeds.
	matchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"task-1","ref":"FAC-1","title":"t","status":"done","priority":"low","projectId":"p","labels":[]}`))
		}
	}))
	defer matchServer.Close()
	kp2 := NewKaneoProvider(matchServer.URL, "p", false)
	if err := kp2.UpdateStatus(context.Background(), "task-1", "done"); err != nil {
		t.Fatalf("matching readback must succeed: %v", err)
	}
}

func TestConformance_Memory_ReadbackAndNormalize(t *testing.T) {
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "m1", Ref: "FAC-1", Title: "t", Status: StatusToDo, ProjectID: "p"})

	if err := mp.UpdateStatus(context.Background(), "m1", "In Progress"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := mp.GetTask(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusInProgress {
		t.Errorf("status=%q want in-progress", got.Status)
	}

	// Inject drift by mutating map behind the provider's back mid-flight is not
	// possible without a hook; prove VerifyStatusReadback rejects mismatch via
	// the shared helper (already covered) and that unknown status never becomes done.
	if NormalizeStatus("mystery-column") == StatusDone {
		t.Fatal("unknown must not map to done")
	}
}

func TestConformance_StatusNormalization_AdapterBoundary(t *testing.T) {
	// Kaneo DTO statuses normalize at dtoToTask.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"t1","ref":"FAC-1","title":"t","status":"Open","priority":"high","projectId":"p","labels":[]}`))
	}))
	defer server.Close()
	kp := NewKaneoProvider(server.URL, "p", false)
	task, err := kp.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusToDo {
		t.Errorf("Open → to-do, got %q", task.Status)
	}

	// Unknown never becomes done.
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"t2","ref":"FAC-2","title":"t","status":"blocked-waiting","priority":"low","projectId":"p","labels":[]}`))
	}))
	defer server2.Close()
	kp2 := NewKaneoProvider(server2.URL, "p", false)
	task2, err := kp2.GetTask(context.Background(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if task2.Status == StatusDone || task2.Status == StatusToDo {
		t.Fatalf("unknown status must not map to done/to-do: %q", task2.Status)
	}
	if !strings.HasPrefix(task2.Status, "unknown:") {
		t.Errorf("want unknown: prefix, got %q", task2.Status)
	}
}

func TestConformance_CLI_ErrorBodyInStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub test is POSIX-only")
	}
	dir := t.TempDir()
	stub := `#!/bin/sh
echo '{"error":"board API unavailable"}'
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	_, err := kp.GetTask(context.Background(), "x")
	if err == nil {
		t.Fatal("CLI 0-exit with error JSON body must fail")
	}
	if !strings.Contains(err.Error(), "board API unavailable") && !strings.Contains(fmt.Sprint(err), "error") {
		// Ensure we did not silently succeed; message may be wrapped.
		t.Logf("error was: %v", err)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError in chain, got %T: %v", err, err)
	}
}

// Review reject FAC-124 #1: non-empty duplicate page must hard-fail (not soft success).
func TestConformance_Pagination_DuplicatePageHardError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub test is POSIX-only")
	}
	dir := t.TempDir()
	// Page 1: one task. Page 2+: same task again (non-empty, zero fresh).
	stub := `#!/bin/sh
echo '[{"id":"id-1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p1","labels":[]}]'
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	tasks, err := kp.ListTasks(context.Background(), "p1", "")
	if err == nil {
		t.Fatalf("duplicate non-empty page must hard-fail, got %d tasks", len(tasks))
	}
	if !errors.Is(err, ErrDuplicatePage) {
		t.Fatalf("want ErrDuplicatePage, got %v", err)
	}
	// Non-vacuity: empty-terminated listing still succeeds.
	emptyOK := `#!/bin/sh
page=1
prev=""
for a in "$@"; do
  if [ "$prev" = "--page" ]; then page="$a"; fi
  prev="$a"
done
if [ "$page" = "1" ]; then
  echo '[{"id":"id-1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p1","labels":[]}]'
else
  echo '[]'
fi
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(emptyOK), 0o755); err != nil {
		t.Fatal(err)
	}
	tasks, err = kp.ListTasks(context.Background(), "p1", "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("empty termination must succeed: tasks=%d err=%v", len(tasks), err)
	}
}

// Review reject FAC-124 #1: page cap without empty termination must hard-fail.
func TestConformance_Pagination_CapWithoutEmptyHardError(t *testing.T) {
	// GitHub: every page returns a unique issue → never empty within DefaultMaxListPages.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Unique number per page so freshCount > 0 always.
		fmt.Fprintf(w, `[{"number":%s,"title":"t","body":"","state":"open","labels":[],"created_at":"2026-08-01T12:00:00Z"}]`, page)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	gp := NewGitHubProvider("tok", "o", "r")
	gp.Client = &http.Client{Transport: &customTripper{targetURL: u}}

	tasks, err := gp.ListTasks(context.Background(), "", "open")
	if err == nil {
		t.Fatalf("page cap without empty must hard-fail, got %d tasks", len(tasks))
	}
	if !errors.Is(err, ErrPaginationCap) {
		t.Fatalf("want ErrPaginationCap, got %v", err)
	}
	// Non-vacuity: empty on page 2 still succeeds (see TestConformance_GitHub_PaginationEmptyPage).
}

// Review reject FAC-124 #2: Azure ListTasks must not swallow GetTask hydration errors.
func TestConformance_Azure_ListTasksHydrationErrorPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/org/proj/_apis/wit/wiql":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"workItems":[{"id":1},{"id":2}]}`))
		case r.URL.Path == "/org/proj/_apis/wit/workitems/1":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"id": 1,
				"fields": {
					"System.Title": "ok",
					"System.Description": "",
					"System.State": "Active",
					"System.WorkItemType": "Task",
					"Microsoft.VSTS.Common.Priority": 2,
					"System.CreatedDate": "2026-08-01T12:00:00Z"
				}
			}`))
		case r.URL.Path == "/org/proj/_apis/wit/workitems/2":
			// Hydration failure for second item.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ap := NewAzureDevOpsProvider(ts.URL+"/org", "proj", "pat")
	tasks, err := ap.ListTasks(context.Background(), "proj", "")
	if err == nil {
		t.Fatalf("partial hydration must fail, got %d tasks", len(tasks))
	}
	if !strings.Contains(err.Error(), "hydrate") && !strings.Contains(err.Error(), "2") {
		t.Logf("error: %v", err)
	}
	// Non-vacuity: when all items hydrate, ListTasks succeeds (covered by
	// TestAzureDevOpsProvider_GetTaskAndListTasks). Prove we don't return the
	// partial first item on error.
	if len(tasks) != 0 {
		t.Fatalf("on hydrate error must return nil tasks, got %d", len(tasks))
	}
}
