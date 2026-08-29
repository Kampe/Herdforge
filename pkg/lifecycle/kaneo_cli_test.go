package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFakeKaneo(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kaneo")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$FAKE_KANEO_LOG\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kaneo: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("FAKE_KANEO_LOG", logPath)
	t.Setenv("PATH", dir)
	return logPath
}

func writeLifecycleAgents(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write agents fixture: %v", err)
	}
	return path
}

func readKaneoCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake kaneo calls: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLifecycleObserveUsesSupportedKaneoTaskList(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"to-do","labels":[{"name":"worker"}]}]'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)

	eng := &Engine{
		AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:  []string{"worker"},
	}
	summary, err := eng.Point()
	if err != nil {
		t.Fatalf("observe through supported kaneo CLI: %v", err)
	}
	if summary.Todo != 1 || summary.Dispatchable != 1 {
		t.Fatalf("board payload was not observed: %+v", summary)
	}
	calls := readKaneoCalls(t, logPath)
	if len(calls) != 1 || calls[0] != "task list --all --json" {
		t.Fatalf("unexpected kaneo calls: %v", calls)
	}
}

func TestLifecycleActUsesSupportedKaneoStatusAndExactReadback(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","labels":[{"name":"worker"}]}]'
    ;;
  "task status FAC-900 to-do")
    printf '%s\n' '{"ref":"FAC-900","status":"to-do"}'
    ;;
  "task get FAC-900 --json")
    printf '%s\n' '{"ref":"FAC-900","status":"to-do","assigneeName":null}'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)

	eng := &Engine{
		AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:  []string{"worker"},
	}
	summary, err := eng.Act()
	if err != nil {
		t.Fatalf("act through supported kaneo CLI: %v", err)
	}
	if summary.StaleActionsExecuted != 1 || len(summary.Actions) != 1 || !summary.Actions[0].Verified {
		t.Fatalf("reclaim was not verified: %+v", summary)
	}
	want := []string{
		"task list --all --json",
		"task status FAC-900 to-do",
		"task get FAC-900 --json",
	}
	calls := readKaneoCalls(t, logPath)
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected kaneo calls:\n got %v\nwant %v", calls, want)
	}
}

func TestLifecycleObserveFailsClosedWhenKaneoListFails(t *testing.T) {
	installFakeKaneo(t, `
printf 'provider unavailable\n' >&2
exit 42`)
	eng := &Engine{AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`)}
	_, err := eng.Point()
	if err == nil || !strings.Contains(err.Error(), "board unavailable") {
		t.Fatalf("provider failure must fail closed, got %v", err)
	}
}
