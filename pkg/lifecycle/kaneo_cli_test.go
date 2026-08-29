package lifecycle

import (
	"encoding/json"
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

func installFakeReclaimHook(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "reclaim-hook")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$FAKE_RECLAIM_LOG\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake reclaim hook: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("FAKE_RECLAIM_LOG", logPath)
	return path, logPath
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

func TestLifecycleObserveUsesStockKaneoAssigneeSchema(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","assigneeName":"forge-smith","assigneeId":"agent-123","createdAt":"2026-08-28T01:02:03Z","updatedAt":"2026-08-28T04:05:06Z","labels":[{"name":"worker"}]}]'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)

	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{{Name: "smith", Role: "worker", Standing: true}})
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{
		AgentsFile:     writeLifecycleAgents(t, `{"result":{"agents":[{"name":"forge-smith","status":"working","interactive":true}]}}`),
		HoldRoles:      []string{"worker"},
		StandingRoster: &registry,
	}
	summary, err := eng.Point()
	if err != nil {
		t.Fatalf("observe stock kaneo payload: %v", err)
	}
	if summary.InProgress != 1 || summary.StaleInProgress != 0 || len(summary.Utilized) != 1 || summary.Utilized[0] != "FAC-900" {
		t.Fatalf("stock assignee was treated as missing or stale: %+v", summary)
	}

	cards := eng.parseCards(json.RawMessage(`[{"ref":"FAC-900","createdAt":"2026-08-28T01:02:03Z","updatedAt":"2026-08-28T04:05:06Z"}]`))
	if len(cards) != 1 || cards[0].CreatedAtCamel != "2026-08-28T01:02:03Z" || cards[0].UpdatedAtCamel != "2026-08-28T04:05:06Z" {
		t.Fatalf("stock camelCase timestamps were not decoded: %+v", cards)
	}

	calls := readKaneoCalls(t, logPath)
	if len(calls) != 1 || calls[0] != "task list --all --json" {
		t.Fatalf("unexpected kaneo calls: %v", calls)
	}
}

func TestLifecycleActUsesConfiguredReclaimHookAndExactKaneoReadback(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","labels":[{"name":"worker"}]}]'
    ;;
  "task get FAC-900 --json")
    printf '%s\n' '{"ref":"FAC-900","status":"to-do","assigneeName":null}'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)
	reclaimHook, reclaimLog := installFakeReclaimHook(t, `
printf '%s\n' '{"ref":"FAC-900","status":"to-do"}'`)

	eng := &Engine{
		AgentsFile:  writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:   []string{"worker"},
		ReclaimHook: reclaimHook,
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
		"task get FAC-900 --json",
	}
	calls := readKaneoCalls(t, logPath)
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected kaneo calls:\n got %v\nwant %v", calls, want)
	}
	wantHook := "--ref FAC-900 --owner worker --reason stale in-progress owner --return-to-to-do"
	if calls := readKaneoCalls(t, reclaimLog); len(calls) != 1 || calls[0] != wantHook {
		t.Fatalf("unexpected reclaim hook calls: got %v want %q", calls, wantHook)
	}
}

func TestLifecycleActRefusesStaleReclaimWithoutConfiguredHookBeforeMutation(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","labels":[{"name":"worker"}]}]'
    ;;
  *)
    printf 'unexpected mutation/readback: %s\n' "$*" >&2
    exit 64
    ;;
esac`)

	eng := &Engine{
		AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:  []string{"worker"},
	}
	_, err := eng.Act()
	wantErr := "lifecycle: stale reclaim requires a configured repository-approved ReclaimHook; refusing direct board mutation"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("missing hook must fail before mutation: got %v want %q", err, wantErr)
	}
	if calls := readKaneoCalls(t, logPath); len(calls) != 1 || calls[0] != "task list --all --json" {
		t.Fatalf("missing hook attempted a mutation/readback: %v", calls)
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

func TestLifecycleObserveFailsClosedOnKaneoErrorEnvelope(t *testing.T) {
	installFakeKaneo(t, `
printf '%s\n' '{"error":"provider unavailable"}'
exit 0`)
	eng := &Engine{AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`)}
	_, err := eng.Point()
	if err == nil || !strings.Contains(err.Error(), "error envelope") {
		t.Fatalf("exit-zero provider error must fail closed, got %v", err)
	}
}

func TestLifecycleObserveAcceptsDescriptionContainingErrorFieldText(t *testing.T) {
	installFakeKaneo(t, `
printf '%s\n' '[{"ref":"FAC-900","status":"to-do","description":"documents an escaped \"error\": field","labels":[{"name":"worker"}]}]'
exit 0`)
	eng := &Engine{
		AgentsFile: writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:  []string{"worker"},
	}
	summary, err := eng.Point()
	if err != nil {
		t.Fatalf("legitimate description text was rejected as an error envelope: %v", err)
	}
	if summary.Todo != 1 || summary.Dispatchable != 1 {
		t.Fatalf("legitimate board payload was not observed: %+v", summary)
	}
}

func TestLifecycleActFailsClosedOnKaneoStatusErrorEnvelope(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","labels":[{"name":"worker"}]}]'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)
	reclaimHook, _ := installFakeReclaimHook(t, `
printf '%s\n' '{"error":"write failed"}'`)

	eng := &Engine{
		AgentsFile:  writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:   []string{"worker"},
		ReclaimHook: reclaimHook,
	}
	_, err := eng.Act()
	if err == nil || !strings.Contains(err.Error(), "error envelope") {
		t.Fatalf("exit-zero status error must fail closed, got %v", err)
	}
	want := []string{"task list --all --json"}
	if calls := readKaneoCalls(t, logPath); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("status error must stop before readback: got %v want %v", calls, want)
	}
}

func TestLifecycleActFailsClosedOnKaneoReadbackErrorEnvelope(t *testing.T) {
	logPath := installFakeKaneo(t, `
case "$*" in
  "task list --all --json")
    printf '%s\n' '[{"ref":"FAC-900","status":"in-progress","labels":[{"name":"worker"}]}]'
    ;;
  "task get FAC-900 --json")
    printf '%s\n' '{"error":"read failed"}'
    ;;
  *)
    printf 'unexpected args: %s\n' "$*" >&2
    exit 64
    ;;
esac`)
	reclaimHook, _ := installFakeReclaimHook(t, `
printf '%s\n' '{"ref":"FAC-900","status":"to-do"}'`)

	eng := &Engine{
		AgentsFile:  writeLifecycleAgents(t, `{"result":{"agents":[]}}`),
		HoldRoles:   []string{"worker"},
		ReclaimHook: reclaimHook,
	}
	_, err := eng.Act()
	if err == nil || !strings.Contains(err.Error(), "error envelope") {
		t.Fatalf("exit-zero readback error must fail closed, got %v", err)
	}
	want := []string{
		"task list --all --json",
		"task get FAC-900 --json",
	}
	if calls := readKaneoCalls(t, logPath); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected kaneo calls: got %v want %v", calls, want)
	}
}

func TestVerifyReclaimReadbackRejectsStockKaneoAssignee(t *testing.T) {
	eng := &Engine{}
	payload := []byte(`{"ref":"FAC-900","status":"to-do","assigneeName":"forge-smith","assigneeId":"agent-123"}`)
	if eng.verifyReclaimReadback("FAC-900", payload) {
		t.Fatal("assigned stock kaneo readback falsely proved unassigned")
	}
}
