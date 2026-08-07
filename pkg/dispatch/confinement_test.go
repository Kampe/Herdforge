package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/confinement"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// staticRoot implements WorktreeService for confinement unit tests.
type staticRoot struct{ root string }

func (s staticRoot) CreateTaskWorktreeFrom(context.Context, string, string) (*worktree.WorktreeInfo, error) {
	return nil, nil
}
func (s staticRoot) RepoRoot() string { return s.root }

// FAC-190: production confinement gate must execute under Production=true with
// an injected enforcer — package tests must not leave the gate as dead code.
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
		ReceiptDir: filepath.Join(wt, ".herd", "confinement"),
	}
	d := &Dispatcher{
		Production:  true,
		Confinement: enf,
		Worktree:    staticRoot{root: shared},
	}
	req := launch.Request{
		Decision: &router.LaunchDecision{
			Provider: "codex",
			Model:    "m",
			Effort:   "medium",
			Argv:     []string{"codex", "--model", "m"},
		},
	}
	_, prep, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep == nil || !prep.WrapperResolves("codex") || prep.ProfileDigest == "" {
		t.Fatalf("prep incomplete: %+v", prep)
	}
	env, err := prep.TabEnv(wt, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "ZDOTDIR=") {
		t.Fatalf("missing ZDOTDIR: %v", env)
	}

	req2 := launch.Request{
		Decision: &router.LaunchDecision{
			Provider: "ollama",
			Model:    "m",
			Effort:   "medium",
			Argv:     []string{"opencode", "--model", "m"},
		},
	}
	_, prep2, err := d.prepareConfinementOS(req2, &worktree.WorktreeInfo{Path: wt})
	if err != nil {
		t.Fatal(err)
	}
	if !prep2.WrapperResolves("ollama") || !prep2.WrapperResolves("opencode") {
		t.Fatalf("multi-name wrap: %+v", prep2.Names)
	}

	d.Confinement = &confinement.Enforcer{Issuer: issuer, OS: nil}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: wt}); err == nil {
		t.Fatal("nil OS accepted")
	}
}

func TestProductionConfinementRequiresArgv(t *testing.T) {
	d := &Dispatcher{Production: true, Worktree: staticRoot{root: t.TempDir()}}
	req := launch.Request{Decision: &router.LaunchDecision{Provider: "codex"}}
	if _, _, err := d.prepareConfinementOS(req, &worktree.WorktreeInfo{Path: t.TempDir()}); err == nil {
		t.Fatal("empty argv accepted")
	}
}
