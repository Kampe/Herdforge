package herdr

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewLaunchEnvPinsBudget(t *testing.T) {
	env := ReviewLaunchEnv()
	got := map[string]string{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			got[k] = v
		}
	}
	if got["HERD_ROLE"] != "agent" {
		t.Fatalf("HERD_ROLE=%q", got["HERD_ROLE"])
	}
	if got["GOMAXPROCS"] != ReviewBudgetGOMAXPROCS {
		t.Fatalf("GOMAXPROCS=%q want %s", got["GOMAXPROCS"], ReviewBudgetGOMAXPROCS)
	}
	if got["GOFLAGS"] != ReviewBudgetGOFLAGS {
		t.Fatalf("GOFLAGS=%q want %s", got["GOFLAGS"], ReviewBudgetGOFLAGS)
	}
}

func TestMergeBoundedGOFLAGSPreservesUserFlags(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{in: "", want: "-p=1"},
		{in: "-mod=mod", want: "-mod=mod -p=1"},
		{in: "-p=4", want: "-p=1"},
		{in: "-p=4 -mod=mod", want: "-mod=mod -p=1"},
		{in: "-p 4 -mod=mod", want: "-mod=mod -p=1"},
		{in: "-mod=mod -p=1", want: "-mod=mod -p=1"},
	}
	for _, tt := range tests {
		if got := mergeBoundedGOFLAGS(tt.in); got != tt.want {
			t.Errorf("mergeBoundedGOFLAGS(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}

func TestWithReviewVerificationBudgetKeepsExplicitGOFLAGS(t *testing.T) {
	got := withReviewVerificationBudget([]string{"GOFLAGS=-mod=mod"})
	var flags string
	for _, entry := range got {
		if k, v, ok := strings.Cut(entry, "="); ok && k == "GOFLAGS" {
			flags = v
		}
	}
	if !strings.Contains(flags, "-mod=mod") {
		t.Fatalf("user GOFLAGS lost: %v", got)
	}
	if !strings.Contains(flags, "-p=1") {
		t.Fatalf("bounded -p=1 missing from merged GOFLAGS: %v", got)
	}
	if strings.Contains(flags, "-p=4") {
		t.Fatalf("unbounded -p survived: %v", got)
	}
}

func TestReviewTabCreate_InheritsVerificationBudget(t *testing.T) {
	cwd := t.TempDir()
	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string(nil), args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"review"},"root_pane":{"pane_id":"p1","tab_id":"t1","terminal_id":"term_a"}}}`, nil
	}
	tab, err := ReviewTabCreate("wK", "review-fac-607", cwd)
	if err != nil {
		t.Fatalf("ReviewTabCreate: %v", err)
	}
	if tab.Cwd != abs || tab.Pane.TerminalID != "term_a" {
		t.Fatalf("tab=%+v", tab)
	}
	env := envArgs(got)
	if env["GOMAXPROCS"] != "2" {
		t.Fatalf("launch env GOMAXPROCS=%q env=%v", env["GOMAXPROCS"], env)
	}
	if env["GOFLAGS"] != "-p=1" {
		t.Fatalf("launch env GOFLAGS=%q env=%v", env["GOFLAGS"], env)
	}
	if env["HERD_ROLE"] != "agent" {
		t.Fatalf("HERD_ROLE=%q", env["HERD_ROLE"])
	}
	if env["HERD_WORKSPACE"] != "wK" {
		t.Fatalf("HERD_WORKSPACE=%q", env["HERD_WORKSPACE"])
	}
}

func TestReviewTabCreate_RequiresWorkspaceAndCwd(t *testing.T) {
	if _, err := ReviewTabCreate("", "x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("workspace: %v", err)
	}
	if _, err := ReviewTabCreate("wK", "x", ""); err == nil || !strings.Contains(err.Error(), "cwd is required") {
		t.Fatalf("cwd: %v", err)
	}
}

func TestReviewTabCreate_LiveHerdrInheritsBudget(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not on PATH")
	}
	if out, err := exec.Command("herdr", "status", "server").CombinedOutput(); err != nil {
		t.Skipf("herdr server: %v\n%s", err, out)
	}
	t.Setenv("GOFLAGS", "-mod=mod")
	cwd := t.TempDir()
	tab, err := ReviewTabCreate("wK", "fac852-budget-env-proof", cwd)
	if err != nil {
		t.Fatalf("ReviewTabCreate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = exec.Command("herdr", "tab", "close", tab.ID).CombinedOutput()
	})
	ready := false
	for i := 0; i < 20; i++ {
		out, err := exec.Command("herdr", "pane", "process-info", "--pane", tab.Pane.ID).CombinedOutput()
		if err == nil && strings.Contains(string(out), `"shell_pid"`) && !strings.Contains(string(out), `"shell_pid":null`) {
			ready = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !ready {
		t.Fatal("review tab shell never became inspectable")
	}
	marker := "ENVPROOF"
	probe := fmt.Sprintf("printf '%s GOMAXPROCS=%%s GOFLAGS=%%s\\n' \"$GOMAXPROCS\" \"$GOFLAGS\"\n", marker)
	if out, err := exec.Command("herdr", "pane", "send-text", tab.Pane.ID, probe).CombinedOutput(); err != nil {
		t.Fatalf("pane send-text: %v\n%s", err, out)
	}
	wantLine := marker + " GOMAXPROCS=2 GOFLAGS="
	var body string
	for i := 0; i < 25; i++ {
		out, err := exec.Command("herdr", "pane", "read", tab.Pane.ID, "--source", "recent-unwrapped").CombinedOutput()
		if err == nil && strings.Contains(string(out), wantLine) {
			body = string(out)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("pane read never showed executed ENVPROOF line; did not start a model or test suite")
	}
	if !strings.Contains(body, "-p=1") {
		t.Fatalf("live GOFLAGS missing bounded -p=1: %s", body)
	}
	if !strings.Contains(body, "-mod=mod") {
		t.Fatalf("live GOFLAGS dropped user -mod=mod: %s", body)
	}
}
