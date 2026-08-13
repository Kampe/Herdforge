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
			Provider:    "codex",
			Harness:     router.PiHarness,
			Model:       "m",
			Effort:      "medium",
			Argv:        []string{"codex", "--model", "m"},
			HarnessArgv: []string{"pi", "--model", "m", "--thinking", "medium"},
		},
	}
	_, prep, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Production herdr kind is pi — that wrapper is the only intercept that matters.
	if prep == nil || !prep.WrapperResolves(router.PiHarness) || prep.ProfileDigest == "" {
		t.Fatalf("prep incomplete (need pi wrapper): %+v", prep)
	}
	// Session integrity store must sit under shared, never nested in the worktree
	// write grant (round-5 CRITICAL). Empty body / substring-only checks are not enough:
	// a path under wt still contains "confine-sessions" if created there.
	if prep.Session.Root == "" {
		t.Fatal("empty session root")
	}
	if !strings.Contains(prep.Session.Root, "confine-sessions") {
		t.Fatalf("session not under confine-sessions: %s", prep.Session.Root)
	}
	if prep.Session.Root == wt || strings.HasPrefix(prep.Session.Root, wt+string(os.PathSeparator)) {
		t.Fatalf("session nested inside worktree write grant: session=%s wt=%s", prep.Session.Root, wt)
	}
	if !strings.HasPrefix(prep.Session.Root, shared+string(os.PathSeparator)) && prep.Session.Root != shared {
		t.Fatalf("session not under shared root: session=%s shared=%s", prep.Session.Root, shared)
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
			Provider:    "ollama",
			Harness:     router.PiHarness,
			Model:       "m",
			Effort:      "medium",
			Argv:        []string{"opencode", "--model", "m"},
			HarnessArgv: []string{"pi", "--model", "m", "--thinking", "medium"},
		},
	}
	_, prep2, err := d.prepareConfinementOS(req2, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"})
	if err != nil {
		t.Fatal(err)
	}
	if !prep2.WrapperResolves(router.PiHarness) {
		t.Fatalf("pi wrapper missing: %+v", prep2.Names)
	}
	// Extras may also be installed; harness is the hard requirement.
	if !prep2.WrapperResolves("ollama") || !prep2.WrapperResolves("opencode") {
		t.Fatalf("optional multi-name wrap: %+v", prep2.Names)
	}

	d.Confinement = &confinement.Enforcer{Issuer: issuer, OS: nil}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"}); err == nil {
		t.Fatal("nil OS accepted")
	}
}

func TestProductionConfinementRequiresHarness(t *testing.T) {
	d := &Dispatcher{Production: true, Worktree: staticRoot{root: t.TempDir()}}
	req := launch.Request{TaskRef: "FAC-190", LeaseGeneration: 1, Decision: &router.LaunchDecision{Provider: "codex"}}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: t.TempDir()}); err == nil {
		t.Fatal("missing harness accepted")
	}
}

func TestProductionConfinementAcceptsVendorHarness(t *testing.T) {
	shared := t.TempDir()
	issuer, _ := confinement.NewHMACIssuer([]byte("s"))
	d := &Dispatcher{
		Production:  true,
		Confinement: &confinement.Enforcer{Issuer: issuer, OS: &confinement.FakeOS{}},
		Worktree:    staticRoot{root: shared},
	}
	req := launch.Request{
		TaskRef: "FAC-190", LeaseGeneration: 1,
		Decision: &router.LaunchDecision{
			Provider: "codex", Harness: "codex", Model: "m", Effort: "medium",
			Argv: []string{"codex"}, HarnessArgv: []string{"codex"},
		},
	}
	_, prep, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: filepath.Join(shared, "wt")})
	if err != nil {
		t.Fatalf("configured vendor harness rejected: %v", err)
	}
	if prep == nil || !prep.WrapperResolves("codex") {
		t.Fatalf("configured vendor harness wrapper missing: %+v", prep)
	}
}

func TestBindConfinementRequiresSessionGeneration(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	issuer, _ := confinement.NewHMACIssuer([]byte("s"))
	enf := &confinement.Enforcer{Issuer: issuer, OS: &confinement.FakeOS{}}
	d := &Dispatcher{Production: true, Confinement: enf, Worktree: staticRoot{root: shared}}
	req := launch.Request{
		TaskRef: "FAC-190", LeaseGeneration: 7, Repository: "repo",
		// SessionGeneration deliberately 0 — must fail closed (no second mint).
		Decision: &router.LaunchDecision{
			Provider: "codex", Harness: router.PiHarness, Model: "m", Effort: "medium",
			Argv: []string{"codex"}, HarnessArgv: []string{"pi", "--model", "m"},
		},
	}
	_, prep, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"})
	if err != nil {
		t.Fatal(err)
	}
	err = d.bindConfinement(enf, prep, req,
		&provider.Task{Ref: "FAC-190"},
		&config.LaneDef{Name: "smith"},
		&worktree.WorktreeInfo{Path: wt, Branch: "task/fac-190"},
		&DispatchResult{LeaseGeneration: 7},
		&herdr.TabInfo{ID: "t", Pane: herdr.PaneInfo{ID: "p"}},
		"task-fac-190",
	)
	if err == nil {
		t.Fatal("bindConfinement minted a session generation instead of requiring the lifecycle one")
	}
	if !strings.Contains(err.Error(), "session generation") {
		t.Fatalf("want session generation error, got %v", err)
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
			Provider: "codex", Harness: router.PiHarness, Model: "m", Effort: "medium",
			Argv: []string{"codex", "--model", "m"}, HarnessArgv: []string{"pi"},
		},
	}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: filepath.Join(shared, "wt")}); err == nil {
		t.Fatal("zero lease accepted")
	}
}
