package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
)

func TestSourceCleanupNativeCLIDryRunAndActPreservesWorktreeAndBranch(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "source.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "initial").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "https://example.invalid/fixture.git").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	baseSHABytes, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}

	wtRel := ".worktrees/mender-fac794-source-retirement"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-794-source-retirement"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "code.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "code.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "feat: change").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	candidateSHABytes, _ := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	candidateSHA := strings.TrimSpace(string(candidateSHABytes))

	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-794\nCandidate: " + candidateSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reportData)
	reportDigest := hex.EncodeToString(sum[:])

	// Launch receipt
	agentName := "forge-mender-fac794-gem-6774ef2d"
	sessionID := "session-uuid-1234"
	launchReceipt := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         agentName,
		Branch:       branch,
		Worktree:     wtRel,
		CandidateSHA: candidateSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
	}
	launchBytes, _ := json.Marshal(launchReceipt)
	launchReceiptsPath := filepath.Join(root, ".herd/launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchReceiptsPath, append(launchBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write herd.yaml
	herdConfigDir := filepath.Join(root, ".herd")
	if err := os.MkdirAll(herdConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdConfigDir, "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: test-repo\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Register retirement manifest
	manifestsPath := filepath.Join(root, ".herd/source/retirement-manifests.jsonl")
	if err := os.MkdirAll(filepath.Dir(manifestsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	m := herdr.NewSourceRetirementManifest(time.Now(), herdr.SourceRetirementManifest{
		Repository: repository, TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: candidateSHA, BaseSHA: baseSHA, Branch: branch,
		Worktree: wtRel, Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: sessionID, AgentName: agentName, Role: "mender", AgentKind: "opencode",
		ReportArtifact: reportRel, ReportDigest: reportDigest, Generation: "gen-1", Nonce: "nonce-1",
	})
	mBytes, _ := json.Marshal(m)
	if err := os.WriteFile(manifestsPath, append(mBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	tabClosed := false
	t.Cleanup(herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if tabClosed {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"` + agentName + `","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","focused":false,"agent_session":{"value":"` + sessionID + `"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			if tabClosed {
				return `{"result":{"tabs":[]}}`, nil
			}
			return `{"result":{"tabs":[{"tab_id":"wK:t17G","workspace_id":"wK"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			if tabClosed {
				return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
			}
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && (args[1] == "compare-close" || args[1] == "close") {
			tabClosed = true
			return `{"result":{"closed":true}}`, nil
		}
		if len(args) >= 2 && args[0] == "api" && args[1] == "capabilities" {
			return `{"result":{"capabilities":["tab_compare_close"]}}`, nil
		}
		return "", errors.New("unexpected mock Herdr command: " + strings.Join(args, " "))
	}))

	// 1. Dry run
	reportDry, err := runSourceRetirementCleanup(context.Background(), root, true)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if tabClosed {
		t.Fatal("tab closed during dry run")
	}
	if len(reportDry.Candidates) != 1 || !reportDry.Candidates[0].Decision.Eligible || !reportDry.DryRun || reportDry.Retired != 0 {
		t.Fatalf("unexpected dry run report: %+v", reportDry)
	}

	// 2. Act
	tabClosed = false
	reportAct, err := runSourceRetirementCleanup(context.Background(), root, false)
	if err != nil {
		t.Fatalf("act failed: %v", err)
	}
	if !tabClosed {
		t.Fatal("tab was not closed during act")
	}
	if reportAct.Retired != 1 || reportAct.DryRun {
		t.Fatalf("unexpected act report: %+v", reportAct)
	}

	// 3. Verify worktree and branch STILL exist!
	if info, err := os.Stat(wtPath); os.IsNotExist(err) || !info.IsDir() {
		t.Fatalf("source worktree was deleted: %s", wtPath)
	}
	branchCheck, err := exec.Command("git", "-C", root, "rev-parse", "--verify", branch).Output()
	if err != nil || strings.TrimSpace(string(branchCheck)) != candidateSHA {
		t.Fatalf("source branch was deleted or mutated: %v (%s)", err, branchCheck)
	}
}

func TestSourceCleanupNativeAutomaticEnrollmentFromDurableHandoff(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "main.go").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "initial").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "https://example.invalid/fixture.git").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}

	// Create worktree for source lane
	wtRel := ".worktrees/mender-fac794-auto"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-794-auto"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "feature.go"), []byte("package main\n// impl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "feature.go").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "feat: feature impl").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	candidateSHABytes, _ := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	candidateSHA := strings.TrimSpace(string(candidateSHABytes))

	// 1. Write durable handoff report in .herd/reports/FAC-794.md
	reportRel := ".herd/reports/FAC-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportContent := "# FAC-794 Report\nStatus: READY\nCandidate: " + candidateSHA + "\n"
	if err := os.WriteFile(reportPath, []byte(reportContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Write launch receipt in .herd/launch-receipts.jsonl
	launchReceiptsPath := filepath.Join(root, ".herd/launch-receipts.jsonl")
	agentName := "forge-mender-fac794-gem-6774ef2d"
	sessionID := "session-auto-1234"
	lr := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         agentName,
		Branch:       branch,
		Worktree:     wtRel,
		CandidateSHA: candidateSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: sessionID,
	}
	lrBytes, _ := json.Marshal(lr)
	if err := os.WriteFile(launchReceiptsPath, append(lrBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Write herd.yaml
	herdConfigDir := filepath.Join(root, ".herd")
	if err := os.WriteFile(filepath.Join(herdConfigDir, "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Note: Notice that .herd/source/retirement-manifests.jsonl is NOT pre-seeded!
	manifestsPath := filepath.Join(root, ".herd/source/retirement-manifests.jsonl")
	if _, err := os.Stat(manifestsPath); !os.IsNotExist(err) {
		t.Fatal("manifest file should not exist before automatic enrollment")
	}

	tabClosed := false
	t.Cleanup(herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if tabClosed {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"` + agentName + `","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","focused":false,"agent_session":{"value":"` + sessionID + `"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			if tabClosed {
				return `{"result":{"tabs":[]}}`, nil
			}
			return `{"result":{"tabs":[{"tab_id":"wK:t17G","workspace_id":"wK"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			if tabClosed {
				return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
			}
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && (args[1] == "compare-close" || args[1] == "close") {
			tabClosed = true
			return `{"result":{"closed":true}}`, nil
		}
		if len(args) >= 2 && args[0] == "api" && args[1] == "capabilities" {
			return `{"result":{"capabilities":["tab_compare_close"]}}`, nil
		}
		return "", errors.New("unexpected mock Herdr command: " + strings.Join(args, " "))
	}))

	// Run public source retirement cleanup
	report, err := runSourceRetirementCleanup(context.Background(), root, false)
	if err != nil {
		t.Fatalf("runSourceRetirementCleanup failed: %v", err)
	}
	if report.Retired != 1 || report.DryRun {
		t.Fatalf("expected 1 retired lane, got %+v", report)
	}
	if !tabClosed {
		t.Fatal("expected live agent tab to be closed")
	}

	// Invariant verification:
	// 1. Worktree must STILL exist on disk!
	if info, err := os.Stat(wtPath); os.IsNotExist(err) || !info.IsDir() {
		t.Fatalf("source worktree was deleted: %s", wtPath)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "feature.go")); err != nil {
		t.Fatalf("source worktree files were removed: %v", err)
	}

	// 2. Branch must STILL exist in git with untouched candidate SHA!
	branchCheck, err := exec.Command("git", "-C", root, "rev-parse", "--verify", branch).Output()
	if err != nil || strings.TrimSpace(string(branchCheck)) != candidateSHA {
		t.Fatalf("source branch was deleted or mutated: %v (%s)", err, branchCheck)
	}

	// 3. Manifests, phases, and receipts files must now exist in .herd/source/
	if _, err := os.Stat(manifestsPath); err != nil {
		t.Fatalf("expected retirement manifests file to be created: %v", err)
	}
	receiptsPath := filepath.Join(root, herdr.SourceRetirementReceiptsFile)
	if _, err := os.Stat(receiptsPath); err != nil {
		t.Fatalf("expected retirement receipts file to be created: %v", err)
	}
}
