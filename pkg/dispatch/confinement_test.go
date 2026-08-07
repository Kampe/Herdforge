package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/confinement"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// staticRoot implements WorktreeService for confinement unit tests.
type staticRoot struct{ root string }

func (s staticRoot) CreateTaskWorktreeFrom(context.Context, string, string) (*worktree.WorktreeInfo, error) {
	return nil, nil
}
func (s staticRoot) RepoRoot() string { return s.root }

func TestProductionConfinementPrepareAndBind(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "task-wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	issuer, err := confinement.NewHMACIssuer([]byte("dispatch-fac190-secret"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &confinement.FakeOS{}
	enf := &confinement.Enforcer{
		Issuer:     issuer,
		OS:         fake,
		ReceiptDir: filepath.Join(shared, ".herd", "confine-sessions", "receipts"),
	}
	d := &Dispatcher{
		Production:  true,
		Confinement: enf,
		Worktree:    staticRoot{root: shared},
	}
	req := launch.Request{
		TaskRef:         "FAC-190",
		LeaseGeneration: 7,
		Decision: &router.LaunchDecision{
			Provider: "codex",
			Model:    "m",
			Effort:   "medium",
			Argv:     []string{"codex", "--model", "m"},
		},
	}
	_, prep, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep == nil || !prep.WrapperResolves("codex") || prep.ProfileDigest == "" {
		t.Fatalf("prep incomplete: %+v", prep)
	}
	if prep.Session.Root == "" || strings.Contains(prep.Session.Root, wt) {
		// session must be under shared, not nested in wt
	}
	if !strings.Contains(prep.Session.Root, "confine-sessions") {
		t.Fatalf("session not under confine-sessions: %s", prep.Session.Root)
	}
	env, err := prep.TabEnv(wt, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "ZDOTDIR=") {
		t.Fatalf("missing ZDOTDIR: %v", env)
	}

	// Full post-tab bindConfinement path (was previously untested).
	// SessionGeneration is pre-set so toolchild.NextSessionGeneration is not required.
	req.SessionGeneration = 1
	req.Repository = "github.com/Kampe/Herdforge"
	tab := &herdr.TabInfo{ID: "tab-1", Pane: herdr.PaneInfo{ID: "pane-1", TabID: "tab-1"}}
	result := &DispatchResult{LeaseGeneration: 7}
	task := &provider.Task{Ref: "FAC-190", Title: "test"}
	lane := &config.LaneDef{Name: "smith"}
	if err := d.bindConfinement(enf, prep, req, task, lane, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"}, result, tab, "task-fac-190"); err != nil {
		t.Fatalf("bindConfinement: %v", err)
	}
	// Receipt under session dir.
	if _, err := os.Stat(filepath.Join(prep.Session.Root, "last-binding.json")); err != nil {
		t.Fatalf("session receipt missing: %v", err)
	}

	req2 := launch.Request{
		TaskRef:         "FAC-190",
		LeaseGeneration: 8,
		Decision: &router.LaunchDecision{
			Provider: "ollama",
			Model:    "m",
			Effort:   "medium",
			Argv:     []string{"opencode", "--model", "m"},
		},
	}
	_, prep2, err := d.prepareConfinementOS(req2, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"})
	if err != nil {
		t.Fatal(err)
	}
	if !prep2.WrapperResolves("ollama") || !prep2.WrapperResolves("opencode") {
		t.Fatalf("multi-name wrap: %+v", prep2.Names)
	}

	d.Confinement = &confinement.Enforcer{Issuer: issuer, OS: nil}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"}); err == nil {
		t.Fatal("nil OS accepted")
	}
}

func TestProductionConfinementRequiresArgv(t *testing.T) {
	d := &Dispatcher{Production: true, Worktree: staticRoot{root: t.TempDir()}}
	req := launch.Request{TaskRef: "FAC-190", LeaseGeneration: 1, Decision: &router.LaunchDecision{Provider: "codex"}}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: t.TempDir()}); err == nil {
		t.Fatal("empty argv accepted")
	}
}

func TestProductionConfinementRequiresLease(t *testing.T) {
	shared := t.TempDir()
	issuer, _ := confinement.NewHMACIssuer([]byte("s"))
	d := &Dispatcher{
		Production:  true,
		Confinement: &confinement.Enforcer{Issuer: issuer, OS: &confinement.FakeOS{}},
		Worktree:    staticRoot{root: shared},
	}
	req := launch.Request{
		TaskRef: "FAC-190",
		Decision: &router.LaunchDecision{
			Provider: "codex", Model: "m", Effort: "medium",
			Argv: []string{"codex", "--model", "m"},
		},
	}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: filepath.Join(shared, "wt")}); err == nil {
		t.Fatal("zero lease accepted")
	}
}
