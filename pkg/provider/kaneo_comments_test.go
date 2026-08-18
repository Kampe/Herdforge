package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestKaneoListComments_UsesAuthenticatedCLIProfile(t *testing.T) {
	previous := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = previous })
	var gotArgs []string
	kaneoRunCLI = func(_ context.Context, name string, args ...string) (*CLIResult, error) {
		gotArgs = append([]string{name}, args...)
		return &CLIResult{Stdout: []byte(`[{"content":"verdict approved"},{"content":"merge ready"}]`)}, nil
	}

	k := NewKaneoProvider("https://kanban.example", "project", true)
	comments, err := k.ListComments(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 2 || comments[0] != "verdict approved" || comments[1] != "merge ready" {
		t.Fatalf("comments = %#v", comments)
	}
	want := []string{"kaneo", "task", "comment", "list", "task-1", "--json"}
	if len(gotArgs) != len(want) {
		t.Fatalf("CLI args = %#v, want %#v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("CLI args = %#v, want %#v", gotArgs, want)
		}
	}
}

func TestKaneoListComments_HTTPFallbackAuthenticatesAndReadsExactRoute(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/task/task-1/comment" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"content":"approved"}]`))
	}))
	t.Cleanup(server.Close)

	k := NewKaneoProvider(server.URL, "project", false)
	k.APIKey = "operator-key"
	k.KeyTrustedOrigin = server.URL
	comments, err := k.ListComments(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if gotAuth != "Bearer operator-key" {
		t.Fatalf("Authorization = %q, want bound operator credential", gotAuth)
	}
	if strings.Join(comments, ",") != "approved" {
		t.Fatalf("comments = %#v", comments)
	}
}

func TestKaneoListComments_CLIRejectsStructuredErrorBody(t *testing.T) {
	previous := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = previous })
	kaneoRunCLI = func(context.Context, string, ...string) (*CLIResult, error) {
		return &CLIResult{Stdout: []byte(`{"error":"not authenticated"}`)}, nil
	}

	_, err := NewKaneoProvider("https://kanban.example", "project", true).ListComments(context.Background(), "task-1")
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("structured CLI error must be hard failure, got %v", err)
	}
}

func TestNewFromHerdConfig_UseCLIControlsVerdictReadback(t *testing.T) {
	previous := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = previous })
	var calls int
	kaneoRunCLI = func(_ context.Context, name string, args ...string) (*CLIResult, error) {
		calls++
		if name != "kaneo" || strings.Join(args, " ") != "task comment list task-1 --json" {
			t.Fatalf("unexpected CLI call: %s %v", name, args)
		}
		return &CLIResult{Stdout: []byte(`[{"content":"approved"}]`)}, nil
	}

	tp, err := NewFromHerdConfig(&config.Config{TaskProvider: config.TaskProvider{
		Type: "kaneo", APIURL: "https://repository-configured.example", ProjectID: "project", UseCLI: true,
		Enabled: []string{"kaneo"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	comments, err := tp.(CommentReader).ListComments(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if calls != 1 || len(comments) != 1 || comments[0] != "approved" {
		t.Fatalf("calls=%d comments=%#v", calls, comments)
	}
}
