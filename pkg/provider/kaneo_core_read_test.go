package provider

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestCoreTaskReadConfiguredRouteAndCapability(t *testing.T) {
	old := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = old })
	var calls [][]string
	kaneoRunCLI = func(_ context.Context, name string, args ...string) (*CLIResult, error) {
		if name != "kaneo-core" {
			t.Fatalf("wrong executable: %s", name)
		}
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"task", "get", "--help"}) {
			return &CLIResult{Stdout: []byte("Usage: kaneo-core task get [OPTIONS] <ID>\n  --core  Core evidence")}, nil
		}
		if !reflect.DeepEqual(args, []string{"task", "get", "FAC-1", "--json", "--core", "--project", "p1"}) {
			return nil, errors.New("unexpected legacy or unscoped read")
		}
		return &CLIResult{Stdout: []byte(`{"id":"t1","ref":"FAC-1","projectId":"p1","labels":["bounded"]}`)}, nil
	}
	tp, err := NewFromHerdConfig(&config.Config{TaskProvider: config.TaskProvider{Type: "kaneo", ProjectID: "p1", UseCLI: true, CoreTaskReads: true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		task, err := tp.GetTask(context.Background(), "FAC-1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(task.Labels, []string{"bounded"}) {
			t.Fatal(task.Labels)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("capability should be checked once; calls=%v", calls)
	}
}

func TestCoreTaskReadCapabilityFailureDoesNotReadTask(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		err        error
	}{{"legacy", "Usage: kaneo-core task get [OPTIONS] <ID>\n --full", nil}, {"errorjson", `{"error":"--core unavailable"}`, nil}, {"failed", "", errors.New("unavailable")}} {
		t.Run(tc.name, func(t *testing.T) {
			old := kaneoRunCLI
			t.Cleanup(func() { kaneoRunCLI = old })
			calls := 0
			kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
				calls++
				if !reflect.DeepEqual(args, []string{"task", "get", "--help"}) {
					t.Fatalf("task read after refusal: %v", args)
				}
				return &CLIResult{Stdout: []byte(tc.help)}, tc.err
			}
			k := &KaneoProvider{UseCLI: true, CoreTaskReads: true, ProjectID: "p1"}
			if _, err := k.getTaskOnce(context.Background(), "FAC-1"); err == nil {
				t.Fatal("accepted unsupported capability")
			}
			if calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}

func TestCoreTaskReadRejectsIncompleteIdentity(t *testing.T) {
	for _, body := range []string{`{"id":"t1","ref":"FAC-1","projectId":"wrong","labels":[]}`, `{"id":"t1","projectId":"p1","labels":[]}`, `{"id":"t1","ref":"FAC-1","projectId":"p1"}`, `{"id":"t1","ref":"FAC-1","projectId":"p1","labels":null}`, `{"id":"t1","ref":"FAC-1","projectId":"p1","labels":[""]}`} {
		t.Run(body, func(t *testing.T) {
			old := kaneoRunCLI
			t.Cleanup(func() { kaneoRunCLI = old })
			kaneoRunCLI = func(_ context.Context, _ string, args ...string) (*CLIResult, error) {
				if strings.Contains(strings.Join(args, " "), "--help") {
					return &CLIResult{Stdout: []byte("Usage: kaneo-core task get\n --core")}, nil
				}
				return &CLIResult{Stdout: []byte(body)}, nil
			}
			k := &KaneoProvider{UseCLI: true, CoreTaskReads: true, ProjectID: "p1"}
			if _, err := k.getTaskOnce(context.Background(), "FAC-1"); err == nil {
				t.Fatal("accepted incomplete evidence")
			}
		})
	}
}

func TestCoreCapabilityBuiltExecutable(t *testing.T) {
	if _, err := exec.LookPath(kaneoCoreReadExecutable); err != nil {
		t.Skip("optional core reader is not built")
	}
	k := &KaneoProvider{CoreTaskReads: true, ProjectID: "fixture-project"}
	if err := k.requireCoreRead(context.Background()); err != nil {
		t.Fatal(err)
	}
}
