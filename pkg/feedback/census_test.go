package feedback

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

func TestDefaultFleetStateDirScopesForeignRepo(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	got := defaultFleetStateDir(filepath.Join("/tmp", "chainseer"))
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "chainseer", "herd")
	if got != want {
		t.Fatalf("foreign repo state dir=%q, want %q", got, want)
	}
}

func TestDefaultFleetStateDirUsesProjectNameAcrossWorktrees(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("HERD_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", stateHome)

	worktreeA := t.TempDir()
	worktreeB := t.TempDir()
	for _, root := range []string{worktreeA, worktreeB} {
		herdDir := filepath.Join(root, ".herd")
		if err := os.Mkdir(herdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		config := []byte("version: \"1\"\nproject:\n  name: \"chainseer\"\ntask_provider:\n  type: \"kaneo\"\n")
		if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), config, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gotA := defaultFleetStateDir(worktreeA)
	gotB := defaultFleetStateDir(worktreeB)
	want := filepath.Join(stateHome, "chainseer", "herd")
	if gotA != want || gotB != want {
		t.Fatalf("worktree state dirs = %q and %q, want both %q", gotA, gotB, want)
	}
}

func TestDefaultFleetStateDirHonorsExplicitOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "shared-herd-state")
	t.Setenv("HERD_STATE_DIR", override)
	if got := defaultFleetStateDir(filepath.Join("/tmp", "chainseer")); got != override {
		t.Fatalf("state override=%q, want %q", got, override)
	}
}

func TestActiveRequestedLanesDropsRetiredWorkers(t *testing.T) {
	requested := []string{"coordinator", "forge-worker-cha-1633", "review-harvest-supervisor"}
	agents := []herdr.AgentEntry{
		{Name: "coordinator", Workspace: "wB"},
		{Name: "review-harvest-supervisor", Workspace: "wB"},
		{Name: "other-repo-worker", Workspace: "wC"},
	}
	got := ActiveRequestedLanes(requested, agents, "wB")
	want := []string{"coordinator", "review-harvest-supervisor"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active lanes=%v, want %v", got, want)
	}
}

func TestActiveRequestedLanesForRosterExcludesEphemeralReviewer(t *testing.T) {
	requested := []string{"coordinator", "review-harvest-supervisor", "review-fix-grok46-dc9f7f1"}
	agents := []herdr.AgentEntry{
		{Name: "coordinator", Workspace: "wB"},
		{Name: "review-harvest-supervisor", Workspace: "wB"},
		{Name: "review-fix-grok46-dc9f7f1", Workspace: "wB"},
	}
	got := ActiveRequestedLanesForRoster(requested, agents, "wB", []string{"coordinator", "review-harvest-supervisor"})
	want := []string{"coordinator", "review-harvest-supervisor"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roster lanes=%v, want %v", got, want)
	}
}

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

func TestMissingIgnoresRepliesOutsideDenominator(t *testing.T) {
	got := Missing([]string{"active"}, []string{"active", "retired", "retired"})
	if len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
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
	for _, want := range []string{"FLEET_FEEDBACK E1", "HERD_LANE=<your-lane> herd send orchestrator", "NONE", "durable inbox record"} {
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
				{Name: "coordinator-1", Workspace: "wF", Status: "idle", PaneID: "coordinator-pane"},
				{Name: "other-workspace-lane", Workspace: "wOther", Status: "idle"},
			}, nil
		},
		DurableMail:   func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		Wake:          func(ctx context.Context, lane, nudge string) error { return nil },
		AdmissionGate: func(ctx context.Context) error { return nil },
		Stdout:        &stdout,
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

func TestRunCensusDedupesRotatedStandingIdentity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	var sent []string
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{
				{Name: "coordinator-1", Workspace: "wF", Status: "idle", PaneID: "coordinator-pane"},
				{Name: "standing-reviewer", Workspace: "wF", TabID: "tab", PaneID: "pane", Session: herdr.AgentSession{Value: "old-session"}},
				{Name: "standing-reviewer", Workspace: "wF", TabID: "tab", PaneID: "pane", Session: herdr.AgentSession{Value: "new-session"}},
			}, nil
		},
		DurableMail:   func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		Wake:          func(ctx context.Context, lane, nudge string) error { return nil },
		AdmissionGate: func(ctx context.Context) error { return nil },
		Stdout:        &bytes.Buffer{},
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, []string{"standing-reviewer"}) {
		t.Fatalf("sent = %v, want one reply identity after rotation", sent)
	}
	state, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Lanes, []string{"standing-reviewer"}) {
		t.Fatalf("persisted lanes = %v, want one standing identity", state.Lanes)
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

func TestRunRefusesWhenNoLiveCoordinatorExists(t *testing.T) {
	var stderr bytes.Buffer
	opts := Options{
		StateDir: t.TempDir(), MailDir: t.TempDir(), Workspace: "wF",
		AdmissionGate: func(context.Context) error { return nil },
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{{Name: "coordinator", Workspace: "wF", Status: "done", PaneID: "retired"}}, nil
		},
		Stderr: &stderr,
	}
	if err := Run(context.Background(), opts); err == nil {
		t.Fatal("Run() must refuse a census without a live coordinator")
	}
	if !strings.Contains(stderr.String(), "no live coordinator") {
		t.Fatalf("stderr = %q, want no-live-coordinator diagnostic", stderr.String())
	}
}

func TestRunRejectsExplicitDeadCoordinator(t *testing.T) {
	opts := Options{
		StateDir: t.TempDir(), MailDir: t.TempDir(), Workspace: "wF", Coordinator: "coordinator",
		AdmissionGate: func(context.Context) error { return nil },
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{{Name: "coordinator", Workspace: "wF", Status: "done", PaneID: "retired"}}, nil
		},
	}
	if err := Run(context.Background(), opts); err == nil {
		t.Fatal("Run() must reject an explicitly supplied dead coordinator")
	}
}

func TestRunPropagatesCoordinatorAgentListError(t *testing.T) {
	sentinel := errors.New("agent inventory unavailable")
	opts := Options{
		StateDir: t.TempDir(), MailDir: t.TempDir(), Workspace: "wF",
		AdmissionGate: func(context.Context) error { return nil },
		ListAgents:    func() ([]herdr.AgentEntry, error) { return nil, sentinel },
	}
	if err := Run(context.Background(), opts); !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
}

func TestRunAdmissionGateRejectionSkipsNewCensusWithoutError(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	var sent []string
	var stdout bytes.Buffer
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		AdmissionGate: func(ctx context.Context) error { return winddown.ErrWinddownActive },
		DurableMail:   func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		Stdout:        &stdout,
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil (admission rejection is not a command failure)", err)
	}
	if len(sent) != 0 {
		t.Fatalf("admission rejection must not send any envelope, sent=%v", sent)
	}
	if !strings.Contains(stdout.String(), "herd-feedback: fleet admission rejected, not starting a new census") {
		t.Fatalf("stdout missing admission-rejected line: %s", stdout.String())
	}
	if _, err := os.Stat(StatePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("admission rejection must not persist a new census, stat err=%v", err)
	}
}

// The default AdmissionGate wires to the fleet's real posture authority
// (pkg/winddown), not a permissive no-op: `herd winddown on` (or simply
// never having run `herd winddown off`) must stop this census from waking
// idle lanes, matching every other fleet-capacity-engaging command.
func TestRunDefaultAdmissionGateHonorsRealWinddownState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	statePath := filepath.Join(t.TempDir(), "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", statePath)

	stateDir := t.TempDir()
	var sent []string
	var stdout bytes.Buffer
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		ListAgents: func() ([]herdr.AgentEntry, error) {
			return []herdr.AgentEntry{
				{Name: "coordinator-1", Workspace: "wF", Status: "idle", PaneID: "coordinator-pane"},
				{Name: "smith", Workspace: "wF", Status: "idle"},
			}, nil
		},
		DurableMail: func(ctx context.Context, to, summary, body string) error { sent = append(sent, to); return nil },
		// Kept hermetic: no real herdr wake call from a unit test.
		Wake:   func(ctx context.Context, lane, nudge string) error { return nil },
		Stdout: &stdout,
	}
	// No winddown state file exists yet: missing state is deliberately
	// rejected, same as cmd/herd's requireFleetAdmission.
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if len(sent) != 0 {
		t.Fatalf("missing winddown state must refuse a new census, sent=%v", sent)
	}
	message := stdout.String()
	for _, want := range []string{statePath, "herd wind-down off", "--reason initialized"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing-state admission message = %q, want it to contain %q", message, want)
		}
	}

	a, err := winddown.New(statePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), false, "test-actor", "test", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if !reflect.DeepEqual(sent, []string{"smith"}) {
		t.Fatalf("sent = %v, want [smith] once winddown is explicitly disabled", sent)
	}
}

// A failed enumeration must never be indistinguishable from a genuinely
// empty fleet: persisting a zero-lane census here would report 0/0 "fully
// replied" on every subsequent cycle, masking the outage instead of
// surfacing it.
func TestRunAgentListFailureSkipsCycleRatherThanOpeningFalseEmptyCensus(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	stateDir := t.TempDir()
	sentinel := errors.New("herdr unavailable")
	var stderr bytes.Buffer
	opts := Options{
		Interval: 1800, Grace: 600, StateDir: stateDir, MailDir: t.TempDir(),
		Workspace: "wF", Coordinator: "coordinator-1", Now: fixedNow(now),
		AdmissionGate: func(ctx context.Context) error { return nil },
		ListAgents:    func() ([]herdr.AgentEntry, error) { return nil, sentinel },
		Stderr:        &stderr,
	}
	if err := Run(context.Background(), opts); !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
	if strings.Contains(stderr.String(), "WARN agent list unavailable") {
		t.Fatalf("stderr retained the swallowed agent-list warning: %s", stderr.String())
	}
	if _, err := os.Stat(StatePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("a failed enumeration must not persist a false empty census, stat err=%v", err)
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
		DurableMail:   func(ctx context.Context, to, summary, body string) error { return os.ErrPermission },
		AdmissionGate: func(ctx context.Context) error { return nil },
		Stderr:        &bytes.Buffer{},
	}
	if err := Run(context.Background(), opts); err == nil {
		t.Fatal("a failed durable send must fail the whole census, not silently drop the lane")
	}
	if _, err := os.Stat(StatePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("a fatal durable-send failure must not persist a false census, err=%v", err)
	}
}

func TestCensusTickInterval(t *testing.T) {
	cases := []struct {
		tickSec int
		want    int
	}{
		{15, 120}, // 30 min / 15 s = 120 ticks (default forge loop)
		{30, 60},  // 30 min / 30 s = 60 ticks
		{60, 30},  // 30 min / 60 s = 30 ticks
		{1800, 1}, // tick == census interval → every tick
		{3600, 1}, // tick > census interval → clamp to 1
		{0, 1},    // non-positive → clamp to 1
		{-5, 1},   // negative → clamp to 1
	}
	for _, c := range cases {
		got := CensusTickInterval(c.tickSec)
		if got != c.want {
			t.Fatalf("CensusTickInterval(%d) = %d, want %d", c.tickSec, got, c.want)
		}
	}
}

func TestEpochKnownTreatsWakeOnlyEpochAsVoid(t *testing.T) {
	stateDir := t.TempDir()
	if err := Save(stateDir, &CensusState{Epoch: "20260817T060000Z", RequestedAtEpoch: 1, Lanes: []string{"smith"}}); err != nil {
		t.Fatal(err)
	}
	known, err := EpochKnown(stateDir, "20260817T060000Z")
	if err != nil || !known {
		t.Fatalf("known epoch=%v err=%v", known, err)
	}
	ghost, err := EpochKnown(stateDir, "20260817T061837Z")
	if err != nil || ghost {
		t.Fatalf("ghost epoch=%v err=%v, want false", ghost, err)
	}
}
