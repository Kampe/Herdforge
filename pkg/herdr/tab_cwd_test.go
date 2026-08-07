package herdr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTabCreateRequiresWorkspace(t *testing.T) {
	_, err := TabCreate(TabCreateOptions{Label: "x", Cwd: "/tmp"})
	if err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("expected workspace required error, got %v", err)
	}
}

func TestTabCreateForTask_RequiresCwd(t *testing.T) {
	_, err := TabCreateForTask("w1", "task-fac-1", "", true)
	if err == nil || !strings.Contains(err.Error(), "cwd is required") {
		t.Fatalf("expected cwd required error, got %v", err)
	}
	_, err = TabCreateForTask("w1", "task-fac-1", "   ", true)
	if err == nil || !strings.Contains(err.Error(), "cwd is required") {
		t.Fatalf("expected trimmed blank cwd required error, got %v", err)
	}
}

func TestTabCreateForTask_PassesAbsoluteCwd(t *testing.T) {
	tmp := t.TempDir()
	absTmp, err := filepath.Abs(tmp)
	if err != nil {
		t.Fatalf("filepath.Abs temp: %v", err)
	}

	old := runHerdr
	defer func() { runHerdr = old }()
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string(nil), args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"task-fac-1","cwd":"` + absTmp + `"}}}`, nil
	}

	tab, err := TabCreateForTask("wABC", "task-fac-1", tmp, true)
	if err != nil {
		t.Fatalf("TabCreateForTask: %v", err)
	}
	if tab.Cwd != absTmp {
		t.Fatalf("tab.Cwd = %q, want exact abs %q", tab.Cwd, absTmp)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--workspace wABC") {
		t.Fatalf("missing workspace in args: %v", got)
	}
	if !strings.Contains(joined, "--cwd "+absTmp) {
		t.Fatalf("missing absolute cwd in args: %v", got)
	}
	if !strings.Contains(joined, "--no-focus") {
		t.Fatalf("missing no-focus in args: %v", got)
	}
}

func TestConsumptionProven(t *testing.T) {
	cases := []struct {
		baseline, observed string
		want               bool
	}{
		{"idle", "working", true},
		{"", "working", true},
		{"done", "working", true},
		// A bare idle->done is NOT proof: a freshly launched agent renders its
		// UI and can settle straight to done without processing the prompt.
		// Observed twice on grok lanes (FAC-174, FAC-172) — delivery reported
		// healthy while the pane sat at an empty prompt with only its dispatch
		// anchor commit. done counts only via ConsumptionProvenSeen, once the
		// agent has actually been seen working.
		{"idle", "done", false},
		{"", "done", false},
		{"working", "done", true},
		// R3: same-state sequences are not consumption proof
		{"working", "working", false},
		{"done", "done", false},
		{"idle", "idle", false},
		{"working", "idle", false},
		{"idle", "blocked", false},
		{"", "idle", false},
	}
	for _, c := range cases {
		got := ConsumptionProven(c.baseline, c.observed)
		if got != c.want {
			t.Errorf("ConsumptionProven(%q, %q) = %v, want %v", c.baseline, c.observed, got, c.want)
		}
	}

	// done IS proof once the agent was actually seen working — the normal
	// finish, and the repair case where a busy agent takes follow-up work.
	if !ConsumptionProvenSeen("idle", "done", true) {
		t.Error("idle->working->done must prove consumption")
	}
	if ConsumptionProvenSeen("idle", "done", false) {
		t.Error("idle->done without ever working must NOT prove consumption")
	}
	if !ConsumptionProvenSeen("working", "done", false) {
		t.Error("a prompt delivered to an already-working agent that finishes is proof")
	}
}

func TestDeliverAndProve_VerifiedConsumption(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	calls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			calls++
			// After prompt, flip to working so receipt proves consumption.
			st := "idle"
			if calls > 1 {
				st = "working"
			}
			return `{"result":{"agents":[{"name":"task-fac-1","agent_status":"` + st + `","pane_id":"p1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			return "ok", nil
		}
		return "", nil
	}

	rec, err := DeliverAndProve("task-fac-1", "BUILD FAC-1", 5*time.Second)
	if err != nil {
		t.Fatalf("DeliverAndProve: %v", err)
	}
	if !rec.Consumed || !rec.Verified || rec.FinalStatus != "working" {
		t.Fatalf("receipt: %+v", rec)
	}
	if rec.SequenceToken != "idle->working" {
		t.Fatalf("sequence = %q", rec.SequenceToken)
	}
}

func TestDeliverAndProve_WorkingToWorkingRejected(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			// Already working; stays working — cannot prove a new prompt was consumed.
			return `{"result":{"agents":[{"name":"task-fac-1","agent_status":"working","pane_id":"p1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			return "ok", nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
			return "ok", nil
		}
		return "", nil
	}

	rec, err := DeliverAndProve("task-fac-1", "BUILD AGAIN", 3*time.Second)
	if err == nil {
		t.Fatal("working→working must not prove consumption")
	}
	if rec == nil || rec.Consumed {
		t.Fatalf("must not claim consumed: %+v", rec)
	}
	if rec.BaselineStatus != "working" || rec.FinalStatus != "working" {
		t.Fatalf("expected working→working receipt, got %+v", rec)
	}
	if !strings.Contains(err.Error(), "working→working") && !strings.Contains(err.Error(), "not proof") {
		t.Fatalf("error should cite non-proof sequence: %v", err)
	}
}

func TestDeliverAndProve_DoneToDoneRejected(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"t","agent_status":"done"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			return "ok", nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
			return "ok", nil
		}
		return "", nil
	}

	rec, err := DeliverAndProve("t", "x", 3*time.Second)
	if err == nil || rec == nil || rec.Consumed {
		t.Fatalf("done→done must fail; rec=%+v err=%v", rec, err)
	}
}

func TestDeliverAndProve_PromptFailure(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"t","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			return "dead pane", errors.New("exit 1")
		}
		return "", errors.New("unexpected")
	}

	rec, err := DeliverAndProve("t", "x", time.Second)
	if err == nil {
		t.Fatal("expected prompt error")
	}
	if rec == nil || rec.Consumed {
		t.Fatalf("failed receipt must not claim consumed: %+v", rec)
	}
}

func TestTabCreateForTask_RelativeDotResolvesToAbs(t *testing.T) {
	wantAbs, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("filepath.Abs(.): %v", err)
	}

	old := runHerdr
	defer func() { runHerdr = old }()
	var got []string
	calls := 0
	runHerdr = func(args ...string) (string, error) {
		calls++
		got = append([]string(nil), args...)
		return `{"result":{"tab":{"tab_id":"t-dot","label":"task-dot","cwd":"` + wantAbs + `"}}}`, nil
	}

	tab, err := TabCreateForTask("wDOT", "task-dot", ".", true)
	if err != nil {
		t.Fatalf("TabCreateForTask(.): %v", err)
	}
	if calls != 1 {
		t.Fatalf("runHerdr calls = %d, want 1", calls)
	}
	if tab.Cwd != wantAbs {
		t.Fatalf("tab.Cwd = %q, want %q", tab.Cwd, wantAbs)
	}
	// Non-vacuous: emitted --cwd must equal filepath.Abs("."), not the relative ".".
	cwdFlag := ""
	for i := 0; i < len(got)-1; i++ {
		if got[i] == "--cwd" {
			cwdFlag = got[i+1]
			break
		}
	}
	if cwdFlag == "" {
		t.Fatalf("missing --cwd in args: %v", got)
	}
	if cwdFlag != wantAbs {
		t.Fatalf("emitted --cwd = %q, want exact abs %q", cwdFlag, wantAbs)
	}
	if cwdFlag == "." {
		t.Fatalf("emitted relative cwd %q; must resolve at caller", cwdFlag)
	}
}

func TestTabCreateForTask_RejectsMissingAndNonDirectory(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	calls := 0
	runHerdr = func(args ...string) (string, error) {
		calls++
		return `{"result":{"tab":{"tab_id":"should-not","label":"x"}}}`, nil
	}

	missing := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := TabCreateForTask("w1", "task-missing", missing, true)
	if err == nil {
		t.Fatal("expected missing cwd error")
	}
	if calls != 0 {
		t.Fatalf("runHerdr called %d times on missing cwd; want 0", calls)
	}

	// Non-directory path (a regular file).
	f, err := os.CreateTemp(t.TempDir(), "not-a-dir-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	filePath := f.Name()
	_ = f.Close()

	_, err = TabCreateForTask("w1", "task-file", filePath, true)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected not-a-directory error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("runHerdr called %d times on non-dir cwd; want 0", calls)
	}
}
