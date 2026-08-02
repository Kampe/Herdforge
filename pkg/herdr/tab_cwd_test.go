package herdr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTabCreate_RequiresWorkspace(t *testing.T) {
	_, err := TabCreate(TabCreateOptions{Label: "x", Cwd: "/tmp/wt"})
	if err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("expected workspace required error, got %v", err)
	}
}

func TestTabCreateForTask_RequiresCwd(t *testing.T) {
	_, err := TabCreateForTask("w1", "task-fac-1", "", true)
	if err == nil || !strings.Contains(err.Error(), "cwd is required") {
		t.Fatalf("expected cwd required error, got %v", err)
	}
}

func TestTabCreate_PassesCwdAndWorkspace(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string{}, args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"task-fac-1"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
	}

	tab, err := TabCreateForTask("wABC", "task-fac-1", "/repo/.herd/worktrees/fac-1", true)
	if err != nil {
		t.Fatalf("TabCreateForTask: %v", err)
	}
	if tab.Cwd != "/repo/.herd/worktrees/fac-1" {
		t.Fatalf("tab.Cwd = %q", tab.Cwd)
	}
	if tab.ID != "t1" || tab.Pane.ID != "p1" {
		t.Fatalf("unexpected tab: %+v", tab)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--workspace wABC") {
		t.Fatalf("missing workspace in args: %v", got)
	}
	if !strings.Contains(joined, "--cwd /repo/.herd/worktrees/fac-1") {
		t.Fatalf("missing cwd in args: %v", got)
	}
	if !strings.Contains(joined, "--no-focus") {
		t.Fatalf("missing no-focus in args: %v", got)
	}
}

func TestDeliverAndProve_UnverifiedSubmit(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)

	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"task-fac-1","agent_status":"idle","pane_id":"p1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			return "ok", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}

	rec, err := DeliverAndProve("task-fac-1", "do work", false, time.Second)
	if err != nil {
		t.Fatalf("DeliverAndProve: %v", err)
	}
	if !rec.Consumed || rec.Verified {
		t.Fatalf("receipt: %+v", rec)
	}
	if rec.BaselineStatus != "idle" {
		t.Fatalf("baseline = %q", rec.BaselineStatus)
	}
	if rec.SequenceToken != "idle->submitted" {
		t.Fatalf("sequence = %q", rec.SequenceToken)
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

	rec, err := DeliverAndProve("task-fac-1", "BUILD FAC-1", true, 5*time.Second)
	if err != nil {
		t.Fatalf("DeliverAndProve: %v", err)
	}
	if !rec.Consumed || !rec.Verified || rec.FinalStatus != "working" {
		t.Fatalf("receipt: %+v", rec)
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

	rec, err := DeliverAndProve("t", "x", false, time.Second)
	if err == nil {
		t.Fatal("expected prompt error")
	}
	if rec == nil || rec.Consumed {
		t.Fatalf("failed receipt must not claim consumed: %+v", rec)
	}
}
