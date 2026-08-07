package feedback

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestMissingIsSetDifferenceAndDeterministic(t *testing.T) {
	got := Missing([]string{"smith", "scout", "assayer"}, []string{"scout"})
	want := []string{"assayer", "smith"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	if all := Missing([]string{"a"}, []string{"a"}); len(all) != 0 {
		t.Fatalf("fully answered census must report nothing missing, got %v", all)
	}
}

func TestDueAndOverdueWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	if !Due(time.Time{}, now, DefaultInterval) {
		t.Fatal("no prior census must be due")
	}
	if Due(now.Add(-5*time.Minute), now, DefaultInterval) {
		t.Fatal("a census inside the interval must not reopen")
	}
	if !Due(now.Add(-31*time.Minute), now, DefaultInterval) {
		t.Fatal("a census past the interval must reopen")
	}
	if Overdue(now.Add(-time.Hour), now, DefaultGrace, 0) {
		t.Fatal("zero missing is never overdue")
	}
	if Overdue(now.Add(-time.Minute), now, DefaultGrace, 2) {
		t.Fatal("inside grace is not overdue")
	}
	if !Overdue(now.Add(-11*time.Minute), now, DefaultGrace, 2) {
		t.Fatal("missing replies past grace must be overdue")
	}
}

// A corrupt census must fail closed. Treating it as a fresh start would drop
// the outstanding request set and report a false all-clear.
func TestCorruptCensusFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt census must not silently become an empty one")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	want := &CensusState{Epoch: "20260806T000000Z", RequestedAtEpoch: 42, Lanes: []string{"smith"}}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if missing, err := Load(t.TempDir()); err != nil || missing.Epoch != "" {
		t.Fatalf("absent census must load empty: %+v %v", missing, err)
	}
}

// Active agents must not be interrupted; settled ones need the nudge.
func TestNeedsWakeOnlyForSettledAgents(t *testing.T) {
	for _, s := range []string{"idle", "done", "blocked", "unknown", ""} {
		if !NeedsWake(s) {
			t.Fatalf("settled status %q must be woken", s)
		}
	}
	for _, s := range []string{"working", "starting"} {
		if NeedsWake(s) {
			t.Fatalf("active status %q must not be interrupted", s)
		}
	}
}

func TestRequestBodyNamesTheCountableReplyShape(t *testing.T) {
	body := RequestBody("E1", "orchestrator")
	for _, want := range []string{"FLEET_FEEDBACK E1", "HERD_LANE=<your-lane> bin/herd-mail send orchestrator", "NONE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("request body missing %q: %s", want, body)
		}
	}
}

func TestSelftestPasses(t *testing.T) {
	if err := Selftest(); err != nil {
		t.Fatalf("Selftest() = %v, want nil", err)
	}
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRunFastPathSkipsInsideInterval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	if err := Save(stateDir, &CensusState{Epoch: "E0", RequestedAtEpoch: now.Add(-60 * time.Second).Unix(), Lanes: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		DurableMail: func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if len(sent) != 0 {
		t.Fatalf("fast path must not send a new envelope, sent=%v", sent)
	}
}

func TestRunOpensCensusWhenDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	if err := Save(stateDir, &CensusState{Epoch: "E0", RequestedAtEpoch: now.Add(-2 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	var stdout bytes.Buffer
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{
				{Name: "smith", Workspace: "wF", Status: "idle"},
				{Name: "scout", Workspace: "wF", Status: "working"},
				{Name: "coordinator-1", Workspace: "wF", Status: "idle"},
				{Name: "other-workspace-lane", Workspace: "wOther", Status: "idle"},
			}, nil
		},
		DurableMail: func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		Wake:        func(ctx context.Context, lane, nudge string) error { return nil },
		Stdout:      &stdout,
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	want := []string{"smith", "scout"}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v, want %v (excludes coordinator and other workspaces)", sent, want)
	}
	if !strings.Contains(stdout.String(), "requested epoch=") || !strings.Contains(stdout.String(), "lanes=2 durable=yes") {
		t.Fatalf("stdout missing closing line: %s", stdout.String())
	}
	got, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Lanes, want) {
		t.Fatalf("persisted lanes = %v, want %v", got.Lanes, want)
	}
}

func TestRunWindDownSkipsNewCensus(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	if err := Save(stateDir, &CensusState{Epoch: "E0", RequestedAtEpoch: now.Add(-2 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "wind-down")
	if err := os.WriteFile(sentinel, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sent []string
	var stdout bytes.Buffer
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(), WindDown: sentinel,
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		DurableMail: func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		Stdout:      &stdout,
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if len(sent) != 0 {
		t.Fatalf("wind-down must not send any envelope, sent=%v", sent)
	}
	if !strings.Contains(stdout.String(), "herd-feedback: wind-down active, not starting a new census") {
		t.Fatalf("stdout missing wind-down line: %s", stdout.String())
	}
	got, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != "E0" {
		t.Fatalf("wind-down must not overwrite the outstanding census, got epoch=%s", got.Epoch)
	}
}

func TestRunReportsFeedbackMissingPastGrace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	mailDir := t.TempDir()
	epoch := "E1"
	if err := Save(stateDir, &CensusState{Epoch: epoch, RequestedAtEpoch: now.Add(-601 * time.Second).Unix(), Lanes: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	reply := `{"id":1,"type":"message","from":"b","to":"coordinator-1","timestamp":"2026-01-01T00:00:00Z","summary":"FLEET_FEEDBACK E1 b","message":"blocker=NONE"}` + "\n"
	if err := os.WriteFile(filepath.Join(mailDir, "coordinator-1.jsonl"), []byte(reply), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	opts := Options{
		// Large interval: the fast path must still report the outstanding
		// census before returning.
		Interval: 999999, Grace: 600, StateDir: stateDir, MailDir: mailDir,
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		Stdout: &stdout, Stderr: &stderr,
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "epoch=E1 replies=1/2 missing=a") {
		t.Fatalf("stdout missing ranking line: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "herd-feedback: FEEDBACK_MISSING after 600s: a") {
		t.Fatalf("stderr missing FEEDBACK_MISSING: %s", stderr.String())
	}
}

func TestRunWorkspaceUnresolvedRefusesFalseEmptyCensus(t *testing.T) {
	// This test's own process may itself be running as a herd fleet worker
	// with HERD_WORKSPACE set in its ambient environment; neutralize it so
	// the unresolved-workspace path under test is not bypassed by the env
	// override tier.
	t.Setenv("HERD_WORKSPACE", "")
	var stderr bytes.Buffer
	opts := Options{
		StateDir: t.TempDir(), MailDir: t.TempDir(),
		ListWorkspaces: func() ([]herdr.WorkspaceEntry, error) { return nil, nil },
		Stderr:         &stderr,
	}
	err := Run(context.Background(), opts)
	if err != ErrWorkspaceUnresolved {
		t.Fatalf("Run() error = %v, want ErrWorkspaceUnresolved", err)
	}
	if got := strings.TrimSpace(stderr.String()); got != "herd-feedback: workspace unresolved; refusing a false empty census" {
		t.Fatalf("stderr = %q, want exact refusal text", got)
	}
	if _, err := os.Stat(StatePath(opts.StateDir)); !os.IsNotExist(err) {
		t.Fatalf("workspace-unresolved must write nothing, state file err=%v", err)
	}
}

func TestRunDurableSendFailureIsFatal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{{Name: "smith", Workspace: "wF", Status: "idle"}}, nil
		},
		DurableMail: func(ctx context.Context, to, summary, body string) error { return os.ErrPermission },
		Stderr:      &bytes.Buffer{},
	}
	if err := Run(context.Background(), opts); err == nil {
		t.Fatal("a failed durable send must fail the whole census, not silently drop the lane")
	}
	if _, err := os.Stat(StatePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("a fatal durable-send failure must not persist a false census, err=%v", err)
	}
}
