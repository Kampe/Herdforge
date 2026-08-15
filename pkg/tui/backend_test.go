package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/budget"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// --- Mock implementations ---

type mockStatusQuerier struct {
	ds  DaemonStatus
	err error
}

func (m mockStatusQuerier) QueryStatus() (DaemonStatus, error) {
	return m.ds, m.err
}

type mockTaskQuerier struct {
	tasks []*provider.Task
	err   error
}

func (m mockTaskQuerier) ListTasks(_ context.Context, _ string) ([]*provider.Task, error) {
	return m.tasks, m.err
}

type mockBudgetQuerier struct {
	bs  BudgetSnapshot
	err error
}

func (m mockBudgetQuerier) QueryBudget() (BudgetSnapshot, error) {
	return m.bs, m.err
}

type mockClaimQuerier struct {
	claims     []ClaimInfo
	claimed    bool
	claimedMap map[string]bool
	err        error
}

func (m mockClaimQuerier) ActiveClaims(_ context.Context) ([]ClaimInfo, error) {
	return m.claims, m.err
}

func (m mockClaimQuerier) IsClaimed(_ context.Context, ref string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.claimedMap != nil {
		return m.claimedMap[ref], nil
	}
	return m.claimed, nil
}

type mockFleetQuerier struct {
	lanes   []LaneInfo
	label   string
	fleetErr error
}

func (m mockFleetQuerier) Lanes() []LaneInfo {
	return m.lanes
}

func (m mockFleetQuerier) FleetStatus() (string, error) {
	return m.label, m.fleetErr
}

// --- Backend offline / IsOffline tests ---

func TestOfflineBackend_IsOffline(t *testing.T) {
	b := OfflineBackend()
	if !b.IsOffline() {
		t.Fatal("OfflineBackend should report IsOffline=true")
	}
}

func TestNilBackend_IsOffline(t *testing.T) {
	var b *Backend
	if !b.IsOffline() {
		t.Fatal("nil Backend should report IsOffline=true")
	}
}

func TestBackend_WithOneInterface_NotOffline(t *testing.T) {
	b := &Backend{
		Status: mockStatusQuerier{ds: DaemonStatus{State: "ok"}},
	}
	if b.IsOffline() {
		t.Fatal("backend with Status interface should not be offline")
	}
}

// --- Adapter tests ---

func TestBudgetAdapter_WithRealManager(t *testing.T) {
	bm := budget.NewBudgetManager(10.0)
	_, err := bm.RecordUsage("gpt-4o", 1000, 500)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	a := BudgetAdapter{Manager: bm}
	bs, err := a.QueryBudget()
	if err != nil {
		t.Fatalf("QueryBudget: %v", err)
	}
	if bs.MaxUSD != 10.0 {
		t.Errorf("expected MaxUSD=10.0, got %f", bs.MaxUSD)
	}
	if bs.SpentUSD <= 0 {
		t.Errorf("expected SpentUSD>0, got %f", bs.SpentUSD)
	}
	if bs.Exhausted {
		t.Error("should not be exhausted after small usage")
	}
}

func TestBudgetAdapter_NilManager_ReturnsOffline(t *testing.T) {
	a := BudgetAdapter{}
	_, err := a.QueryBudget()
	if !IsOfflineErr(err) {
		t.Fatalf("expected ErrOffline, got %v", err)
	}
}

func TestBudgetAdapter_Exhausted(t *testing.T) {
	bm := budget.NewBudgetManager(10.0)
	bm.TotalCostUSD = 10.0
	a := BudgetAdapter{Manager: bm}
	bs, err := a.QueryBudget()
	if err != nil {
		t.Fatalf("QueryBudget: %v", err)
	}
	if !bs.Exhausted {
		t.Error("expected exhausted=true when spend equals limit")
	}
}

func TestConfigFleetAdapter_LanesFromConfig(t *testing.T) {
	cfg := &config.Config{
		Lanes: []config.LaneDef{
			{Name: "smith", Role: "worker", Model: "gpt-5.6-luna", Standing: false},
			{Name: "orchestrator", Role: "orchestrator", Model: "claude-opus-5", Standing: true},
		},
	}
	a := ConfigFleetAdapter{Cfg: cfg, FleetLabel: "ok"}
	lanes := a.Lanes()
	if len(lanes) != 2 {
		t.Fatalf("expected 2 lanes, got %d", len(lanes))
	}
	if lanes[0].Name != "smith" || lanes[0].Standing {
		t.Errorf("unexpected lane[0]: %+v", lanes[0])
	}
	if lanes[1].Name != "orchestrator" || !lanes[1].Standing {
		t.Errorf("unexpected lane[1]: %+v", lanes[1])
	}
	label, err := a.FleetStatus()
	if err != nil || label != "ok" {
		t.Errorf("expected label=ok err=nil, got label=%q err=%v", label, err)
	}
}

func TestConfigFleetAdapter_NilConfig_NoLanes(t *testing.T) {
	a := ConfigFleetAdapter{}
	if lanes := a.Lanes(); len(lanes) != 0 {
		t.Fatalf("expected 0 lanes for nil config, got %d", len(lanes))
	}
}

func TestConfigFleetAdapter_FleetError(t *testing.T) {
	wantErr := errors.New("fleet unreachable")
	a := ConfigFleetAdapter{FleetErr: wantErr}
	_, err := a.FleetStatus()
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected fleet error, got %v", err)
	}
}

// --- REPL with live mock backend tests ---

func TestREPL_Status_WithLiveBackend(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Status: mockStatusQuerier{ds: DaemonStatus{
			State:     "ok",
			LastError: "",
			UpdatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		}},
	})
	out, _ := repl.Eval("status", nil)
	if !strings.Contains(out, "Daemon Status: ok") {
		t.Errorf("expected 'ok' state in output, got: %s", out)
	}
	if !strings.Contains(out, "2026-01-01 12:00:00") {
		t.Errorf("expected formatted timestamp, got: %s", out)
	}
}

func TestREPL_Status_BackendError_NoFabricatedSuccess(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Status: mockStatusQuerier{err: errors.New("daemon unreachable")},
	})
	out, _ := repl.Eval("status", nil)
	if strings.Contains(out, "ok") && !strings.Contains(out, "ERROR") {
		t.Errorf("must not fabricate success on error, got: %s", out)
	}
	if !strings.Contains(out, "ERROR") {
		t.Errorf("expected ERROR label, got: %s", out)
	}
}

func TestREPL_Budget_WithLiveBackend(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Budget: mockBudgetQuerier{bs: BudgetSnapshot{
			SpentUSD:  0.045,
			MaxUSD:    10.00,
			Tokens:    12000,
			Exhausted: false,
		}},
	})
	out, _ := repl.Eval("budget", nil)
	if !strings.Contains(out, "$0.0450 / $10.0000") {
		t.Errorf("expected formatted budget, got: %s", out)
	}
	if strings.Contains(out, "EXHAUSTED") {
		t.Errorf("should not show EXHAUSTED, got: %s", out)
	}
}

func TestREPL_Budget_Exhausted(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Budget: mockBudgetQuerier{bs: BudgetSnapshot{
			SpentUSD:  10.00,
			MaxUSD:    10.00,
			Exhausted: true,
		}},
	})
	out, _ := repl.Eval("budget", nil)
	if !strings.Contains(out, "[EXHAUSTED]") {
		t.Errorf("expected [EXHAUSTED] tag, got: %s", out)
	}
}

func TestREPL_Budget_BackendError_NoFabricatedSuccess(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Budget: mockBudgetQuerier{err: errors.New("budget store down")},
	})
	out, _ := repl.Eval("budget", nil)
	if !strings.Contains(out, "ERROR") {
		t.Errorf("expected ERROR label, got: %s", out)
	}
	if strings.Contains(out, "$") && !strings.Contains(out, "ERROR") {
		t.Errorf("must not fabricate budget numbers on error, got: %s", out)
	}
}

func TestREPL_Lanes_WithLiveBackend(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Fleet: mockFleetQuerier{
			lanes: []LaneInfo{
				{Name: "smith", Role: "worker", Model: "gpt-5.6-luna", Standing: false},
				{Name: "assayer", Role: "reviewer", Model: "claude-sonnet-5", Standing: false},
			},
			label: "ok",
		},
	})
	out, _ := repl.Eval("lanes", nil)
	if !strings.Contains(out, "Configured Lanes: 2") {
		t.Errorf("expected 2 lanes, got: %s", out)
	}
	if !strings.Contains(out, "smith") || !strings.Contains(out, "assayer") {
		t.Errorf("expected lane names in output, got: %s", out)
	}
	if !strings.Contains(out, "Fleet Status: ok") {
		t.Errorf("expected fleet status, got: %s", out)
	}
}

func TestREPL_Lanes_FleetError_NoFabricatedSuccess(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Fleet: mockFleetQuerier{
			lanes:     []LaneInfo{{Name: "smith", Role: "worker", Model: "m"}},
			fleetErr: errors.New("herdr unreachable"),
		},
	})
	out, _ := repl.Eval("lanes", nil)
	if !strings.Contains(out, "Fleet Status: ERROR") {
		t.Errorf("expected Fleet Status ERROR, got: %s", out)
	}
}

func TestREPL_Tasks_WithLiveBackend(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Tasks: mockTaskQuerier{tasks: []*provider.Task{
			{Ref: "FAC-100", Priority: provider.PriorityHigh, Status: "to-do", Title: "Build X"},
		}},
	})
	out, _ := repl.Eval("tasks", nil)
	if !strings.Contains(out, "Task Queue: 1 pending") {
		t.Errorf("expected 1 pending task, got: %s", out)
	}
	if !strings.Contains(out, "FAC-100") || !strings.Contains(out, "Build X") {
		t.Errorf("expected task details, got: %s", out)
	}
}

func TestREPL_Tasks_BackendError_NoFabricatedSuccess(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Tasks: mockTaskQuerier{err: errors.New("provider timeout")},
	})
	out, _ := repl.Eval("tasks", nil)
	if !strings.Contains(out, "ERROR") {
		t.Errorf("expected ERROR label, got: %s", out)
	}
	if strings.Contains(out, "pending") && !strings.Contains(out, "ERROR") {
		t.Errorf("must not fabricate task list on error, got: %s", out)
	}
}

func TestREPL_Tasks_EmptyQueue(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Tasks: mockTaskQuerier{tasks: nil},
	})
	out, _ := repl.Eval("tasks", nil)
	if !strings.Contains(out, "(no pending tasks)") {
		t.Errorf("expected empty queue message, got: %s", out)
	}
}

func TestREPL_Claim_Claimed(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Claims: mockClaimQuerier{
			claimedMap: map[string]bool{"FAC-45": true},
			claims: []ClaimInfo{{
				Ref:     "FAC-45",
				OwnerID: "w1",
				Role:    "worker",
				Status:  "active",
				ExpiresAt: time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC),
			}},
		},
	})
	out, _ := repl.Eval("claim", []string{"FAC-45"})
	if !strings.Contains(out, "CLAIMED") {
		t.Errorf("expected CLAIMED, got: %s", out)
	}
	if !strings.Contains(out, "w1") || !strings.Contains(out, "worker") {
		t.Errorf("expected owner/role detail, got: %s", out)
	}
}

func TestREPL_Claim_NotClaimed(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Claims: mockClaimQuerier{claimedMap: map[string]bool{}},
	})
	out, _ := repl.Eval("claim", []string{"FAC-99"})
	if !strings.Contains(out, "not claimed") || !strings.Contains(out, "available") {
		t.Errorf("expected 'not claimed (available)', got: %s", out)
	}
}

func TestREPL_Claim_BackendError_NoFabricatedSuccess(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Claims: mockClaimQuerier{err: errors.New("claim store down")},
	})
	out, _ := repl.Eval("claim", []string{"FAC-45"})
	if !strings.Contains(out, "ERROR") {
		t.Errorf("expected ERROR label, got: %s", out)
	}
	if strings.Contains(out, "successfully") {
		t.Errorf("must never say 'successfully' on error, got: %s", out)
	}
}

func TestREPL_Claim_MissingRef(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, nil)
	out, _ := repl.Eval("claim", nil)
	if !strings.Contains(out, "Error: missing task ref") {
		t.Errorf("expected missing-ref error, got: %s", out)
	}
}

// --- Parser seam preservation test ---

func TestParseREPLCommand_PreservedSeam(t *testing.T) {
	cases := []struct {
		input    string
		wantCmd  string
		wantArgs []string
	}{
		{"status", "status", nil},
		{"CLAIM FAC-45", "claim", []string{"FAC-45"}},
		{"  budget  ", "budget", nil},
		{"", "", nil},
		{"help me", "help", []string{"me"}},
	}
	for _, tc := range cases {
		cmd, args := ParseREPLCommand(tc.input)
		if cmd != tc.wantCmd {
			t.Errorf("ParseREPLCommand(%q): cmd=%q want %q", tc.input, cmd, tc.wantCmd)
		}
		if len(args) != len(tc.wantArgs) {
			t.Errorf("ParseREPLCommand(%q): args=%v want %v", tc.input, args, tc.wantArgs)
			continue
		}
		for i, a := range args {
			if a != tc.wantArgs[i] {
				t.Errorf("ParseREPLCommand(%q): args[%d]=%q want %q", tc.input, i, a, tc.wantArgs[i])
			}
		}
	}
}

// --- No-fabricated-success invariant: error paths must never claim success ---

func TestREPL_NoFabricatedSuccess_OnAllErrorPaths(t *testing.T) {
	backend := &Backend{
		Status: mockStatusQuerier{err: errors.New("e")},
		Tasks:  mockTaskQuerier{err: errors.New("e")},
		Budget: mockBudgetQuerier{err: errors.New("e")},
		Claims: mockClaimQuerier{err: errors.New("e")},
		Fleet:  mockFleetQuerier{
			lanes:     []LaneInfo{{Name: "smith", Role: "worker", Model: "m"}},
			fleetErr: errors.New("e"),
		},
	}
	repl := NewREPL(strings.NewReader(""), nil, backend)

	checks := []struct {
		cmd  string
		args []string
	}{
		{"status", nil},
		{"tasks", nil},
		{"budget", nil},
		{"claim", []string{"FAC-1"}},
	}
	for _, c := range checks {
		out, _ := repl.Eval(c.cmd, c.args)
		if strings.Contains(strings.ToLower(out), "success") {
			t.Errorf("ERROR path for %q must not contain 'success': %s", c.cmd, out)
		}
		if !strings.Contains(out, "ERROR") {
			t.Errorf("ERROR path for %q must contain 'ERROR': %s", c.cmd, out)
		}
	}

	// lanes with fleet error should show ERROR too
	out, _ := repl.Eval("lanes", nil)
	if !strings.Contains(out, "ERROR") {
		t.Errorf("ERROR path for lanes must contain 'ERROR': %s", out)
	}
}

// --- Partial backend: some interfaces live, some offline ---

func TestREPL_PartialBackend_LiveStatus_OfflineBudget(t *testing.T) {
	repl := NewREPL(strings.NewReader(""), nil, &Backend{
		Status: mockStatusQuerier{ds: DaemonStatus{State: "ok", UpdatedAt: time.Now()}},
	})
	statusOut, _ := repl.Eval("status", nil)
	if !strings.Contains(statusOut, "Daemon Status: ok") {
		t.Errorf("expected live status, got: %s", statusOut)
	}
	budgetOut, _ := repl.Eval("budget", nil)
	if !strings.Contains(budgetOut, "[OFFLINE]") {
		t.Errorf("expected offline label for budget, got: %s", budgetOut)
	}
}
