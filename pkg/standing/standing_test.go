package standing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func writePrompt(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

func standingFixture(t *testing.T) (repo string, cfg *config.Config) {
	t.Helper()
	repo = t.TempDir()
	// Isolated worktrees under the repo — not the shared root.
	for _, name := range []string{"orch", "harvest"} {
		if err := os.MkdirAll(filepath.Join(repo, ".worktrees", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	orchPrompt := writePrompt(t, repo, ".herd/prompts/orchestrator.md", "STANDING orch packet")
	harvPrompt := writePrompt(t, repo, ".herd/prompts/harvest.md", "STANDING harvest packet")
	// Ephemeral lane must never be raised by standing.
	workerPrompt := writePrompt(t, repo, ".herd/prompts/worker.md", "worker packet")
	cfg = &config.Config{
		Version: "1",
		Project: config.ProjectConfig{Name: "Herdforge", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{
			Type:      "linear",
			ProjectID: "proj",
		},
		Lanes: []config.LaneDef{
			{
				Name: "orch", Role: "orchestrator", Standing: true,
				AgentKind: "pi", Harness: "pi", Prompt: orchPrompt,
				Worktree: ".worktrees/orch", Provider: "claude", Model: "claude-opus-5",
				Effort: "medium", TaskShape: "coordinator",
				Authority: config.AuthorityRead, Capabilities: []config.Capability{config.CapabilityBoardWrite, config.CapabilityNetwork},
				IncompatibleWith: []string{"worker"},
			},
			{
				Name: "worker", Role: "worker", Standing: false,
				AgentKind: "pi", Harness: "pi", Prompt: workerPrompt,
				Worktree: ".worktrees/worker", Provider: "codex", Model: "gpt-5.6-luna",
				Effort: "medium", TaskShape: "implementation",
				Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite},
			},
			{
				Name: "harvest", Role: "harvest", Standing: true,
				AgentKind: "pi", Harness: "pi", Prompt: harvPrompt,
				Worktree: ".worktrees/harvest", Provider: "claude", Model: "claude-sonnet-5",
				Effort: "medium", TaskShape: "bounded",
				Authority: config.AuthorityWrite, Capabilities: []config.Capability{config.CapabilityGitWrite, config.CapabilityBoardWrite},
				IncompatibleWith: []string{"worker", "reviewer"},
			},
		},
	}
	// Config.Validate requires lanes that reference incompatible roles to exist
	// in the roster for those role labels. worker exists; add a reviewer stub.
	cfg.Lanes = append(cfg.Lanes, config.LaneDef{
		Name: "assayer", Role: "reviewer", Standing: false,
		AgentKind: "pi", Harness: "pi", Prompt: workerPrompt,
		Worktree: ".worktrees/assayer", Provider: "claude", Model: "claude-sonnet-5",
		Effort: "medium", TaskShape: "qa",
		Authority: config.AuthorityRead, Capabilities: []config.Capability{config.CapabilityNetwork},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config invalid: %v", err)
	}
	return repo, cfg
}

func baseOpts(t *testing.T, repo string) Options {
	t.Helper()
	return Options{
		RepoRoot: repo,
		ResolveWorkspace: func(string, *config.Config) (string, error) {
			return "wTEST", nil
		},
		ListAgents: func() ([]Agent, error) { return nil, nil },
		AdmitRoute: func(lane *config.LaneDef) (Route, error) {
			return Route{Provider: lane.Provider, Model: lane.Model, Effort: lane.Effort, Harness: lane.Harness}, nil
		},
		RepositoryIdentity: func(*config.Config) string { return "github.com/Kampe/Herdforge" },
		PromptReadable: func(path string) error {
			// Paths are relative to repo; tests chdir or use absolute.
			candidates := []string{path, filepath.Join(repo, path)}
			for _, c := range candidates {
				if st, err := os.Stat(c); err == nil && !st.IsDir() {
					return nil
				}
			}
			return os.ErrNotExist
		},
		AbsPath: func(p string) (string, error) {
			if filepath.IsAbs(p) {
				return filepath.Clean(p), nil
			}
			return filepath.Abs(filepath.Join(repo, p))
		},
	}
}

func TestStandingLanesConfigOrderAndEphemeralExclusion(t *testing.T) {
	_, cfg := standingFixture(t)
	got := StandingLanes(cfg)
	if len(got) != 2 {
		t.Fatalf("standing lanes = %d, want 2 (ephemeral excluded)", len(got))
	}
	if got[0].Name != "orch" || got[1].Name != "harvest" {
		t.Fatalf("order = %s,%s want orch,harvest (config order)", got[0].Name, got[1].Name)
	}
}

func TestSelectUnknownFailsClosed(t *testing.T) {
	_, cfg := standingFixture(t)
	_, err := Select(StandingLanes(cfg), []string{"not-a-lane"})
	if err == nil {
		t.Fatal("unknown selection must fail closed")
	}
}

func TestValidateLaneBlocksMissingPromptAndSharedRoot(t *testing.T) {
	repo, cfg := standingFixture(t)
	orch := StandingLanes(cfg)[0]
	// Missing prompt.
	bad := orch
	bad.Prompt = ".herd/prompts/does-not-exist.md"
	if _, err := ValidateLane(bad, repo, nil, nil); err == nil {
		t.Fatal("missing prompt must block")
	}
	// Shared root: worktree == repo root.
	shared := orch
	shared.Worktree = "."
	if _, err := ValidateLane(shared, repo, func(string) error { return nil }, filepath.Abs); err == nil {
		t.Fatal("shared-root worktree must block")
	}
	// Empty worktree.
	emptyWT := orch
	emptyWT.Worktree = ""
	if _, err := ValidateLane(emptyWT, repo, func(string) error { return nil }, nil); err == nil {
		t.Fatal("empty worktree must block")
	}
	// Missing authority.
	noAuth := orch
	noAuth.Authority = ""
	if _, err := ValidateLane(noAuth, repo, func(string) error { return nil }, nil); err == nil {
		t.Fatal("missing authority must block")
	}
	// Missing capabilities.
	noCap := orch
	noCap.Capabilities = nil
	if _, err := ValidateLane(noCap, repo, func(string) error { return nil }, nil); err == nil {
		t.Fatal("missing capabilities must block")
	}
}

func TestRepeatedRaiseProducesOneOwner(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)

	var creates, starts, prompts int
	live := map[string]Agent{}
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.ListAgents = func() ([]Agent, error) {
		out := make([]Agent, 0, len(live))
		for _, a := range live {
			out = append(out, a)
		}
		return out, nil
	}
	opts.CreateTab = func(ws, label, cwd string) (Tab, error) {
		creates++
		if _, err := os.Stat(cwd); err != nil {
			t.Fatalf("create tab cwd missing: %v", err)
		}
		if filepath.Clean(cwd) == filepath.Clean(repo) {
			t.Fatal("create tab must not use shared root")
		}
		return Tab{ID: "tab-" + label, Label: label, PaneID: "pane-" + label, Cwd: cwd}, nil
	}
	opts.StartAgent = func(tab Tab, agentName string, route Route, lane *config.LaneDef, repository string) error {
		starts++
		if repository == "" {
			return errors.New("repository required")
		}
		live[agentName] = Agent{Name: agentName, Status: "idle", TabID: tab.ID, PaneID: tab.PaneID, Cwd: tab.Cwd}
		return nil
	}
	opts.PromptAgent = func(agentName, text string) error {
		prompts++
		if text == "" {
			return errors.New("empty prompt")
		}
		return nil
	}
	opts.CloseTab = func(string) error { t.Fatal("unexpected close on clean raise"); return nil }

	r1, err := Run(cfg, opts)
	if err != nil {
		t.Fatalf("first raise: %v", err)
	}
	if r1.Raised != 2 || r1.Skipped != 0 {
		t.Fatalf("first raise result = %+v", r1)
	}
	if creates != 2 || starts != 2 || prompts != 2 {
		t.Fatalf("side effects creates=%d starts=%d prompts=%d", creates, starts, prompts)
	}

	// Second raise must be idempotent — no new tabs.
	r2, err := Run(cfg, opts)
	if err != nil {
		t.Fatalf("second raise: %v", err)
	}
	if r2.Raised != 0 || r2.Skipped != 2 {
		t.Fatalf("second raise must skip live owners: %+v", r2)
	}
	if creates != 2 || starts != 2 {
		t.Fatalf("repeated raise mutated fleet: creates=%d starts=%d", creates, starts)
	}
	// Prove the skip is not vacuous: if live map is cleared, raise would run again.
	live = map[string]Agent{}
	r3, err := Run(cfg, opts)
	if err != nil {
		t.Fatalf("third raise after clear: %v", err)
	}
	if r3.Raised != 2 {
		t.Fatalf("after clear, raise must run again (proves skip was real): %+v", r3)
	}
	if creates != 4 {
		t.Fatalf("after clear creates=%d want 4", creates)
	}
}

func TestMissingRouteBlocksBeforeTab(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	creates := 0
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"orch"}
	opts.AdmitRoute = func(*config.LaneDef) (Route, error) {
		return Route{}, errors.New("no healthy provider")
	}
	opts.CreateTab = func(string, string, string) (Tab, error) {
		creates++
		return Tab{}, nil
	}
	r, err := Run(cfg, opts)
	if err == nil {
		t.Fatal("route failure must surface")
	}
	if creates != 0 {
		t.Fatal("route failure must not create tabs")
	}
	if r.Failed != 1 || r.Roles[0].Outcome != OutcomeFailed {
		t.Fatalf("result = %+v", r)
	}
}

func TestMissingPromptBlocksBeforeTab(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	cfg.Lanes[0].Prompt = ".herd/prompts/gone.md"
	creates := 0
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"orch"}
	opts.CreateTab = func(string, string, string) (Tab, error) {
		creates++
		return Tab{}, nil
	}
	r, err := Run(cfg, opts)
	if err == nil {
		t.Fatal("missing prompt must fail")
	}
	if creates != 0 {
		t.Fatal("missing prompt must not create tabs")
	}
	if r.Failed != 1 {
		t.Fatalf("failed=%d", r.Failed)
	}
}

func TestPartialLaunchReconcilesOrphanTab(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	var closed []string
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"harvest"}
	opts.CreateTab = func(ws, label, cwd string) (Tab, error) {
		return Tab{ID: "orphan-tab", Label: label, PaneID: "orphan-pane", Cwd: cwd}, nil
	}
	opts.StartAgent = func(Tab, string, Route, *config.LaneDef, string) error {
		return errors.New("agent start refused")
	}
	opts.CloseTab = func(id string) error {
		closed = append(closed, id)
		return nil
	}
	r, err := Run(cfg, opts)
	if err == nil {
		t.Fatal("start failure must surface")
	}
	if len(closed) != 1 || closed[0] != "orphan-tab" {
		t.Fatalf("orphan tab not reconciled: closed=%v result=%+v", closed, r)
	}
	if r.Roles[0].TabID != "" {
		t.Fatal("failed role must not report a live tab id after reconcile")
	}
}

func TestDryRunNeverTouchesHerdr(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	opts := baseOpts(t, repo)
	opts.Mode = ModeDryRun
	opts.CreateTab = func(string, string, string) (Tab, error) {
		t.Fatal("dry-run must not create tabs")
		return Tab{}, nil
	}
	opts.StartAgent = func(Tab, string, Route, *config.LaneDef, string) error {
		t.Fatal("dry-run must not start agents")
		return nil
	}
	r, err := Run(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Previewed != 2 || r.Raised != 0 {
		t.Fatalf("dry-run result = %+v", r)
	}
	if !strings.Contains(Summary(r), "nothing raised") {
		t.Fatalf("summary = %s", Summary(r))
	}
}

func TestStatusReportsLiveAndMissing(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	opts := baseOpts(t, repo)
	opts.Mode = ModeStatus
	opts.ListAgents = func() ([]Agent, error) {
		return []Agent{
			{Name: "forge-orch", Status: "idle", TabID: "t1", PaneID: "p1", Cwd: filepath.Join(repo, ".worktrees", "orch")},
			// harvest missing
			{Name: "forge-worker", Status: "working", TabID: "t9"}, // ephemeral — ignored
		}, nil
	}
	r, err := Run(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Live != 1 || r.Missing != 1 {
		t.Fatalf("status = %+v", r)
	}
	// Ephemeral must not appear.
	for _, rr := range r.Roles {
		if rr.LaneName == "worker" {
			t.Fatal("status must not report ephemeral workers")
		}
	}
}

func TestShutdownTouchesOnlyStanding(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	var closed []string
	opts := baseOpts(t, repo)
	opts.Mode = ModeShutdown
	opts.ListAgents = func() ([]Agent, error) {
		return []Agent{
			{Name: "forge-orch", Status: "idle", TabID: "t-orch", PaneID: "p-orch"},
			{Name: "forge-harvest", Status: "working", TabID: "t-harv", PaneID: "p-harv"},
			{Name: "forge-worker", Status: "idle", TabID: "t-worker", PaneID: "p-worker"},
			{Name: "task-fac-1", Status: "idle", TabID: "t-task", PaneID: "p-task"},
		}, nil
	}
	opts.CloseTab = func(id string) error {
		closed = append(closed, id)
		return nil
	}
	r, err := Run(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	// idle orch closed; working harvest preserved; worker/task never selected.
	if len(closed) != 1 || closed[0] != "t-orch" {
		t.Fatalf("closed=%v want only t-orch", closed)
	}
	var preserved, closedN int
	for _, rr := range r.Roles {
		switch rr.Outcome {
		case OutcomeClosed:
			closedN++
		case OutcomePreserved:
			preserved++
		}
	}
	if closedN != 1 || preserved != 1 {
		t.Fatalf("shutdown outcomes closed=%d preserved=%d roles=%+v", closedN, preserved, r.Roles)
	}
}

func TestWorkspaceResolutionFailureBlocks(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"orch"}
	opts.ResolveWorkspace = func(string, *config.Config) (string, error) {
		return "", errors.New("unknown workspace")
	}
	opts.CreateTab = func(string, string, string) (Tab, error) {
		t.Fatal("no tab without workspace")
		return Tab{}, nil
	}
	opts.StartAgent = func(Tab, string, Route, *config.LaneDef, string) error {
		t.Fatal("no start without workspace")
		return nil
	}
	r, err := Run(cfg, opts)
	if err == nil {
		t.Fatal("workspace failure must block")
	}
	if r == nil || r.Failed != 1 {
		t.Fatalf("expected one failed role, got %+v", r)
	}
	if !strings.Contains(r.Roles[0].Reason, "unknown workspace") {
		t.Fatalf("reason = %q", r.Roles[0].Reason)
	}
}

func TestCWDMatchesAssignedWorktree(t *testing.T) {
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	var seenCWD string
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"forge-harvest"} // live name form
	opts.CreateTab = func(_, label, cwd string) (Tab, error) {
		seenCWD = cwd
		want, _ := filepath.Abs(filepath.Join(repo, ".worktrees", "harvest"))
		if filepath.Clean(cwd) != filepath.Clean(want) {
			t.Fatalf("cwd=%s want %s", cwd, want)
		}
		return Tab{ID: "t", Label: label, PaneID: "p", Cwd: cwd}, nil
	}
	opts.StartAgent = func(Tab, string, Route, *config.LaneDef, string) error { return nil }
	opts.PromptAgent = func(string, string) error { return nil }
	if _, err := Run(cfg, opts); err != nil {
		t.Fatal(err)
	}
	if seenCWD == "" {
		t.Fatal("create tab never called")
	}
}

// Prove NameHeld is the real gate: unknown status is NOT held, so raise proceeds.
func TestNameHeldGateIsNonVacuous(t *testing.T) {
	if NameHeld("working") != true || NameHeld("unknown") != false || NameHeld("") != false {
		t.Fatal("NameHeld contract drift")
	}
	repo, cfg := standingFixture(t)
	t.Chdir(repo)
	creates := 0
	opts := baseOpts(t, repo)
	opts.Mode = ModeRaise
	opts.Only = []string{"orch"}
	opts.ListAgents = func() ([]Agent, error) {
		// Name present but status not held — must re-attempt raise (or still
		// try create). Shell skips only NameHeld statuses.
		return []Agent{{Name: "forge-orch", Status: "unknown", TabID: "t0"}}, nil
	}
	opts.CreateTab = func(_, label, cwd string) (Tab, error) {
		creates++
		return Tab{ID: "t1", Label: label, PaneID: "p1", Cwd: cwd}, nil
	}
	opts.StartAgent = func(Tab, string, Route, *config.LaneDef, string) error { return nil }
	opts.PromptAgent = func(string, string) error { return nil }
	if _, err := Run(cfg, opts); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("unknown status must not count as live skip; creates=%d", creates)
	}
}
