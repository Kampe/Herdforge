package herdr

import (
	"path/filepath"
	"strings"
	"testing"
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

func TestWithReviewVerificationBudgetKeepsExplicitGOFLAGS(t *testing.T) {
	got := withReviewVerificationBudget([]string{"GOFLAGS=-mod=mod"})
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "GOFLAGS=-mod=mod") {
		t.Fatalf("explicit GOFLAGS lost: %v", got)
	}
	if strings.Contains(joined, "GOFLAGS=-p=1") {
		t.Fatal("explicit GOFLAGS must not be replaced")
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
