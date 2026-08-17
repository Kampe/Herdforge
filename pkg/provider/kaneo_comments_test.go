package provider

import (
	"context"
	"testing"
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
