package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
)

const (
	standingTestWorkerToken = "test-broker-token-min16chars"
	standingTestMintToken   = "test-mint-token-min16chars-xx"
)

func standingBoardWriteLane() *config.LaneDef {
	return &config.LaneDef{
		Name:         "mender-fac776",
		Role:         launch.RecoveryRole,
		Authority:    config.AuthorityWrite,
		Capabilities: []config.Capability{config.CapabilityGitWrite, config.CapabilityBoardWrite},
		Worktree:     ".worktrees/mender-fac776",
	}
}

func standingReadOnlyLane() *config.LaneDef {
	return &config.LaneDef{
		Name:         "docs",
		Role:         launch.ScoutPlannerRole,
		Authority:    config.AuthorityRead,
		Capabilities: []config.Capability{config.CapabilityNetwork},
		Worktree:     ".worktrees/docs",
	}
}

func startStandingTestBroker(t *testing.T) (claimDir string, url string, broker *provider.FenceBroker) {
	t.Helper()
	claimDir = t.TempDir()
	t.Setenv("HERD_CLAIM_DIR", claimDir)
	t.Setenv("HERD_FENCE_COORDINATOR", "")
	t.Setenv("HERD_FENCE_BROKER_MINT_TOKEN", "")
	t.Setenv("HERD_FENCE_ATOMIC_SERVER", "")
	t.Setenv("HERD_ROLE", "orchestrator")
	provider.ProvisionSharedFenceForTest(t, claimDir)
	b, err := provider.StartFenceBroker(provider.FenceBrokerConfig{
		ClaimDir:   claimDir,
		ListenAddr: "127.0.0.1:0",
		Token:      standingTestWorkerToken,
		MintToken:  standingTestMintToken,
	})
	if err != nil {
		t.Fatalf("StartFenceBroker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return claimDir, b.ClientBaseURL(), b
}

func installDummyHarnesses(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"codex", "pi", "grok", "claude"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func standingTestDecision(t *testing.T) *router.LaunchDecision {
	t.Helper()
	isolateLaunchBoundaryRoutingEnv(t)
	installDummyHarnesses(t)
	req := launchBoundaryDecideRequest()
	req.TaskRef = "mender-fac776"
	d, err := testLaunchRouter(t).Decide(req)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStandingBoardWrite_MissingBrokerRefusesBeforeTab(t *testing.T) {
	_, calls := isolateNativeLaunchFixture(t)
	t.Setenv("HERD_FENCE_BROKER_URL", "")
	t.Setenv("HERD_FENCE_BROKER_TOKEN", "")
	t.Setenv("HERD_FENCE_COORDINATOR", "")
	cwd := t.TempDir()
	decision := standingTestDecision(t)
	_, err := standingCreateTab(decision, standingBoardWriteLane(), "wFAKE", "forge-mender-fac776", cwd)
	if err == nil {
		t.Fatal("missing broker must refuse board-write standing launch")
	}
	if !errors.Is(err, provider.ErrWorkerBrokerMissing) {
		t.Fatalf("want ErrWorkerBrokerMissing, got %v", err)
	}
	for _, c := range calls() {
		if strings.Contains(c, "tab create") {
			t.Fatalf("tab created before broker refusal: %v", calls())
		}
	}
}

func TestStandingReadOnly_DoesNotRequireBroker(t *testing.T) {
	t.Setenv("HERD_FENCE_BROKER_URL", "")
	t.Setenv("HERD_FENCE_BROKER_TOKEN", "")
	if err := authorizeStandingBoardWrite(standingReadOnlyLane(), t.TempDir()); err != nil {
		t.Fatalf("read-only lane must skip broker: %v", err)
	}
}

func TestStandingBoardWrite_ValidBrokerConfidentialChildAndMintRefuse(t *testing.T) {
	_, calls := isolateNativeLaunchFixture(t)
	claimDir, url, _ := startStandingTestBroker(t)
	t.Setenv("HERD_FENCE_BROKER_URL", url)
	t.Setenv("HERD_FENCE_BROKER_TOKEN", standingTestWorkerToken)
	cwd := t.TempDir()
	decision := standingTestDecision(t)

	tab, err := standingCreateTab(decision, standingBoardWriteLane(), "wFAKE", "forge-mender-fac776", cwd)
	if err != nil {
		t.Fatalf("valid broker standing launch: %v", err)
	}
	if tab.ID == "" || tab.PaneID == "" {
		t.Fatalf("incomplete tab: %+v", tab)
	}

	for _, c := range calls() {
		if strings.Contains(c, standingTestWorkerToken) || strings.Contains(c, standingTestMintToken) {
			t.Fatalf("secret appeared in herdr argv: %q", c)
		}
	}

	raw, err := os.ReadFile(provider.ConfidentialWorkerBrokerEnvFile(cwd))
	if err != nil {
		t.Fatalf("confidential env.list missing: %v", err)
	}
	if !strings.Contains(string(raw), standingTestWorkerToken) {
		t.Fatal("confidential env.list missing worker token")
	}
	if strings.Contains(string(raw), standingTestMintToken) {
		t.Fatal("confidential env.list leaked mint token")
	}

	t.Setenv("HERD_FENCE_BROKER_URL", "")
	t.Setenv("HERD_FENCE_BROKER_TOKEN", "")
	t.Setenv("HERD_ROOT", cwd)
	child, err := provider.NewFenceBrokerClientFromEnv()
	if err != nil {
		t.Fatalf("child attach from confidential env: %v", err)
	}
	if err := child.Live(context.Background()); err != nil {
		t.Fatalf("child authorized live: %v", err)
	}
	if _, err := provider.NewFenceBrokerMinterFromEnv(); err == nil {
		t.Fatal("child must not mint")
	}
	if err := child.MutateStatus(context.Background(), "tb1", provider.StatusInProgress, 1, "op-child", ""); err == nil {
		t.Fatal("child must not mutate without pre-minted capability")
	}

	receipt := launch.Receipt{
		CreatedAt: time.Now().UTC(), Role: "recovery", TaskShape: "implementation",
		Provider: decision.Provider, Model: decision.Model, Effort: decision.Effort,
		Argv: decision.Argv, Accepted: true, Name: "forge-mender-fac776", CWD: cwd,
	}
	js, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), standingTestWorkerToken) || strings.Contains(string(js), standingTestMintToken) {
		t.Fatalf("receipt leaked secret: %s", js)
	}

	plan, err := launch.Admit(launch.BoundarySpec{
		Decision:  decision,
		Request:   launch.Request{Decision: decision, TaskRef: "mender-fac776", Scope: router.ScopeLane},
		Lane:      standingBoardWriteLane(),
		Workspace: "wFAKE", Label: "forge-mender-fac776", Cwd: cwd,
		Now:          func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
		WriteCapable: func() *bool { v := false; return &v }(),
	})
	if err != nil {
		t.Fatalf("admit serialized plan: %v", err)
	}
	pjs, _ := json.Marshal(plan)
	if strings.Contains(string(pjs), standingTestWorkerToken) {
		t.Fatalf("plan leaked worker token: %s", pjs)
	}
	_ = claimDir
}

func TestStandingBoardWrite_WrongTokenAndForeignVolumeObservedRed(t *testing.T) {
	_, calls := isolateNativeLaunchFixture(t)
	claimDir, url, _ := startStandingTestBroker(t)
	cwd := t.TempDir()
	decision := standingTestDecision(t)
	lane := standingBoardWriteLane()

	t.Run("wrong_token_RED", func(t *testing.T) {
		t.Setenv("HERD_CLAIM_DIR", claimDir)
		t.Setenv("HERD_FENCE_BROKER_URL", url)
		t.Setenv("HERD_FENCE_BROKER_TOKEN", "wrong-worker-token-min16")
		_, err := standingCreateTab(decision, lane, "wFAKE", "forge-mender-fac776", cwd)
		if err == nil {
			t.Fatal("wrong token must refuse before tab create")
		}
		if !errors.Is(err, provider.ErrWorkerBrokerPartial) {
			t.Fatalf("wrong token: %v", err)
		}
	})
	t.Run("foreign_volume_RED", func(t *testing.T) {
		foreign := t.TempDir()
		t.Setenv("HERD_CLAIM_DIR", foreign)
		provider.ProvisionSharedFenceForTest(t, foreign)
		t.Setenv("HERD_FENCE_BROKER_URL", url)
		t.Setenv("HERD_FENCE_BROKER_TOKEN", standingTestWorkerToken)
		_, err := standingCreateTab(decision, lane, "wFAKE", "forge-mender-fac776", cwd)
		if err == nil {
			t.Fatal("foreign volume must refuse before tab create")
		}
		if !errors.Is(err, provider.ErrWorkerBrokerForeign) {
			t.Fatalf("foreign volume: %v", err)
		}
	})
	t.Run("in_process_RED", func(t *testing.T) {
		t.Setenv("HERD_CLAIM_DIR", claimDir)
		t.Setenv("HERD_FENCE_COORDINATOR", "1")
		t.Setenv("HERD_ROLE", "orchestrator")
		t.Setenv("HERD_FENCE_BROKER_URL", "")
		t.Setenv("HERD_FENCE_BROKER_TOKEN", "")
		_, err := standingCreateTab(decision, lane, "wFAKE", "forge-mender-fac776", cwd)
		if err == nil {
			t.Fatal("in-process coordinator must refuse shareable board-write launch")
		}
		if !errors.Is(err, provider.ErrWorkerBrokerInProcess) {
			t.Fatalf("in-process: %v", err)
		}
		if !strings.Contains(err.Error(), "herd fence-broker --claim-dir") {
			t.Fatalf("must name standalone fence-broker: %v", err)
		}
	})
	for _, c := range calls() {
		if strings.Contains(c, "tab create") {
			t.Fatalf("refused launch still created a tab: %v", calls())
		}
	}
}

func TestStandingBoardWrite_AuthMutationTurnsRedThenGreen(t *testing.T) {
	isolateNativeLaunchFixture(t)
	claimDir, url, _ := startStandingTestBroker(t)
	cwd := t.TempDir()
	decision := standingTestDecision(t)
	lane := standingBoardWriteLane()

	t.Setenv("HERD_CLAIM_DIR", claimDir)
	t.Setenv("HERD_FENCE_BROKER_URL", url)
	t.Setenv("HERD_FENCE_BROKER_TOKEN", "wrong-worker-token-min16")
	if _, err := standingCreateTab(decision, lane, "wFAKE", "forge-mender-fac776", cwd); err == nil {
		t.Fatal("auth mutation must be RED with the wrong token")
	}

	t.Setenv("HERD_FENCE_BROKER_TOKEN", standingTestWorkerToken)
	if _, err := standingCreateTab(decision, lane, "wFAKE", "forge-mender-fac776", cwd); err != nil {
		t.Fatalf("restored worker token must be GREEN: %v", err)
	}
}
