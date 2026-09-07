package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

type upFixtureRuntime struct {
	decision      *router.LaunchDecision
	starts, opens int
	startErr      error
	afterStart    func() error
}

func (*upFixtureRuntime) Available() bool { return true }
func (r *upFixtureRuntime) Route(*config.LaneDef) (*router.LaunchDecision, error) {
	return r.decision, nil
}
func (r *upFixtureRuntime) Open(_ *router.LaunchDecision, _ launch.Request, _ *config.LaneDef, _, _, _ string) (*herdr.TabInfo, error) {
	r.opens++
	return &herdr.TabInfo{ID: "wFAKE:t1", Pane: herdr.PaneInfo{ID: "wFAKE:p1"}}, nil
}
func (*upFixtureRuntime) Ready(*herdr.TabInfo) error { return nil }
func (r *upFixtureRuntime) Start(_, _, _, _ string, req launch.Request) error {
	r.starts++
	if req.TaskRef != "worker" || req.Decision != r.decision {
		return errors.New("wrong start binding")
	}
	if r.startErr != nil {
		return r.startErr
	}
	if r.afterStart != nil {
		return r.afterStart()
	}
	return nil
}
func (*upFixtureRuntime) Close(string, *herdr.TabInfo) error {
	return errors.New("unexpected compensation")
}

func upReceiptFixture(t *testing.T) (string, *upFixtureRuntime) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(key, os.Getenv(key))
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := receiptFixture(t)
	installProtocolFakeHerdr(t)
	if out, err := gitCmdForTest(root, "remote", "add", "origin", "https://example.invalid/fixture/up.git"); err != nil {
		t.Fatalf("fixture remote: %v: %s", err, out)
	}
	t.Setenv("HERD_PROJECT_ROOT", root)
	t.Setenv("HERD_CONFIG_PATH", filepath.Join(root, config.DefaultConfigPath))
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	t.Setenv("HERD_WORKSPACE", "wFAKE")
	t.Setenv("HERDR_WORKSPACE_ID", "wFAKE")
	t.Setenv("HERD_WINDDOWN_STATE", filepath.Join(root, ".herd", "winddown.json"))
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "1")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0700); err != nil {
		t.Fatal(err)
	}
	// A configured Claude lane is rerouted to the real router's sealed Codex
	// decision. The command must record that decision, never this preference.
	cfg := `version: "1"
project:
  name: Herdforge
task_provider:
  type: memory
fleet:
  herdr_workspace: wFAKE
lanes:
  - name: worker
    role: worker
    agent_kind: claude
    harness: claude
    provider: claude
    model: claude-sonnet-5
    effort: medium
    prompt: .herd/prompts/worker.md
    task_shape: implementation
    standing: true
    worktree: .
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := initializeWinddownState(); err != nil {
		t.Fatal(err)
	}
	d, err := testLaunchRouter(t).Decide(launchBoundaryDecideRequest())
	if err != nil {
		t.Fatal(err)
	}
	return root, &upFixtureRuntime{decision: d}
}

func TestFAC623UpCommandRecordsResolvedProvenance(t *testing.T) {
	root, runtime := upReceiptFixture(t)
	var out bytes.Buffer
	if err := runUpCommand("worker", runtime, &out); err != nil {
		t.Fatal(err)
	}
	if runtime.starts != 1 || runtime.opens != 1 {
		t.Fatalf("not a raised lane: %+v", runtime)
	}
	receipts := readReceipts(t, root)
	if len(receipts) != 1 {
		t.Fatalf("want one accepted receipt, got %d", len(receipts))
	}
	r := receipts[0]
	if !r.Accepted || r.Lane != "worker" || r.TaskRef != "worker" || r.Branch != "wt/defi-crusader" || r.Provider != "codex" || r.BuilderFamily != "openai" || r.Model != runtime.decision.Model || r.TabID != "wFAKE:t1" || r.PaneID != "wFAKE:p1" {
		t.Fatalf("wrong or missing resolved provenance: %+v", r)
	}
	if !strings.Contains(out.String(), "started:") {
		t.Fatalf("missing success: %s", out.String())
	}
}

func TestFAC623UpCommandRefusesReceiptFailure(t *testing.T) {
	root, runtime := upReceiptFixture(t)
	runtime.afterStart = func() error { return os.Mkdir(filepath.Join(root, ".herd", "launch-receipts.jsonl"), 0700) }
	var out bytes.Buffer
	err := runUpCommand("worker", runtime, &out)
	if err == nil || !strings.Contains(err.Error(), "provenance could not be recorded") {
		t.Fatalf("want receipt failure, got %v", err)
	}
	if runtime.starts != 1 {
		t.Fatalf("did not reach post-start write: %d", runtime.starts)
	}
	if out.Len() != 0 {
		t.Fatalf("reported success without receipt: %s", out.String())
	}
}

func TestFAC623UpCommandDoesNotRecordFailedStart(t *testing.T) {
	root, runtime := upReceiptFixture(t)
	runtime.startErr = errors.New("fixture start refused")
	var out bytes.Buffer
	err := runUpCommand("worker", runtime, &out)
	if !errors.Is(err, runtime.startErr) {
		t.Fatalf("lost start error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "launch-receipts.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("failed start wrote receipt: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("reported success: %s", out.String())
	}
}
