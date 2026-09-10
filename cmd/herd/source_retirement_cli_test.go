package main

import (
	"context"
	"crypto/ed25519"
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

	agentName := "forge-mender-fac794-gem-6774ef2d"
	sessionID := "session-uuid-1234"

	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-794\nTask: FAC-794\nAgent: " + agentName + "\nCandidate: " + candidateSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reportData)
	reportDigest := hex.EncodeToString(sum[:])

	// Launch receipt
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
		HerdrSession: sessionID,
		Repository:   repository,
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
	reportContent := "# FAC-794 Report\nTask: FAC-794\nAgent: forge-mender-fac794-gem-6774ef2d\nStatus: READY\nCandidate: " + candidateSHA + "\n"
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

func TestSourceCleanupNative_ProductionNativeLaunchReceiptEnrollmentAndRetirement(t *testing.T) {
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
	baseSHABytes, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseSHABytes))

	// Create worktree for native mender lane
	laneName := "mender-fac786-native-endpoint"
	agentName := "forge-mender-fac786-nat-b5e8985e"
	sessionID := "session-live-nat-5678"
	wtRel := ".worktrees/mender-fac786-native-endpoint"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-786-native-endpoint"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "fix.go"), []byte("package main\n// fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "fix.go").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "fix: endpoint issue").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	candidateSHABytes, _ := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	candidateSHA := strings.TrimSpace(string(candidateSHABytes))

	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "https://example.invalid/fixture.git").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		t.Fatalf("resolve repository identity: %v", err)
	}

	// Generate receipt signing key and write published verification key to .herd/receipt.pub
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	herdConfigDir := filepath.Join(root, ".herd")
	if err := os.MkdirAll(herdConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdConfigDir, "receipt.pub"), []byte(hex.EncodeToString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Write signed TASK-CONTEXT.json in worktree
	tc := dispatch.TaskContext{
		ProviderType:    "kaneo",
		ProjectID:       "proj-1",
		Repository:      repository,
		Role:            "mender",
		TaskRef:         "FAC-786",
		TaskID:          "task-fac-786",
		Branch:          branch,
		BaseSHA:         baseSHA,
		LeaseID:         "lease-1",
		LeaseGeneration: 1,
		LeaseTaskRef:    "FAC-786",
		SessionID:       sessionID,
		AllowedOps:      []string{"get", "list", "comment"},
		ExpiresAt:       time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	canonical, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, canonical)
	tc.Signature = hex.EncodeToString(sig)

	taskContextData, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "TASK-CONTEXT.json"), taskContextData, 0o600); err != nil {
		t.Fatal(err)
	}
	// Track TASK-CONTEXT.json in git to keep worktree clean
	if out, err := exec.Command("git", "-C", wtPath, "add", "TASK-CONTEXT.json").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "chore: add task context").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	candidateSHABytes, _ = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	candidateSHA = strings.TrimSpace(string(candidateSHABytes))

	// 2. Write durable handoff report in .herd/reports/FAC-786.md
	reportRel := ".herd/reports/FAC-786.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportContent := "# FAC-786 Report\nTask: FAC-786\nAgent: " + agentName + "\nStatus: READY\nCandidate: " + candidateSHA + "\n"
	if err := os.WriteFile(reportPath, []byte(reportContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Write herd.yaml
	if err := os.WriteFile(filepath.Join(herdConfigDir, "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: fixture\ntask_provider:\n  type: memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Write launch receipt from herd up (with CWD, no worktree, no herdr_session, no candidate_sha, TaskRef=laneName)
	launchReceiptsPath := filepath.Join(root, ".herd/launch-receipts.jsonl")
	lr := launch.Receipt{
		Accepted:       true,
		TaskRef:        laneName,
		Lane:           laneName,
		Name:           agentName,
		Role:           "mender",
		TaskShape:      "mender",
		Provider:       "litellm",
		Model:          "gpt-5.6-luna",
		Effort:         "high",
		DecisionDigest: "digest-1",
		PaneID:         "wK:p17G",
		TabID:          "wK:t17G",
		Repository:     repository,
		BuilderFamily:  "openai",
		Branch:         branch,
		CWD:            wtPath,
	}
	lrBytes, _ := json.Marshal(lr)
	if err := os.WriteFile(launchReceiptsPath, append(lrBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	tabClosed := false
	t.Cleanup(herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if tabClosed {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"` + agentName + `","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","cwd":"` + wtPath + `","focused":false,"agent_session":{"value":"` + sessionID + `"}}]}}`, nil
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

	report, err := runSourceRetirementCleanup(context.Background(), root, false)
	if err != nil {
		t.Fatalf("runSourceRetirementCleanup failed: %v", err)
	}
	if report.Retired != 1 || report.Failed != 0 || report.Blocked != 0 {
		t.Fatalf("expected 1 retired candidate, got report: %+v", report)
	}
	if !tabClosed {
		t.Fatal("expected tab to be closed after acting cleanup")
	}

	// Invariants: worktree and branch must remain intact!
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("source worktree was unexpectedly removed: %v", err)
	}
	branchCheck, err := exec.Command("git", "-C", root, "rev-parse", "--verify", branch).Output()
	if err != nil || strings.TrimSpace(string(branchCheck)) != candidateSHA {
		t.Fatalf("source branch was deleted or mutated: %v (%s)", err, branchCheck)
	}
}

func TestDrainSourceRetirementDefaultRefusal(t *testing.T) {
	hooks := defaultDrainActionHooks()
	if hooks.retireSources == nil {
		t.Fatal("retireSources is not set in defaultDrainActionHooks")
	}
	err := hooks.retireSources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no compiled source retirement authority is configured") {
		t.Fatalf("expected refusal error, got %v", err)
	}
}

func TestDrainSourceRetirementWiredInDrainAdapters(t *testing.T) {
	a := &drainAdapters{}
	hooks := a.hooks()
	if hooks.retireSources == nil {
		t.Fatal("retireSources is not wired in drainAdapters.hooks()")
	}
	// Without root authority, it fails closed
	err := hooks.retireSources(context.Background())
	if err == nil || !strings.Contains(err.Error(), "source retirement authority is unavailable") {
		t.Fatalf("expected authority refusal, got %v", err)
	}
}

func TestSourceCleanupNative_HerdYamlAbsenceVsMalformed(t *testing.T) {
	// 1. Absence is harmless (no configured cleanup authority)
	rootClean := t.TempDir()
	report, err := runSourceRetirementCleanup(context.Background(), rootClean, true)
	if err != nil {
		t.Fatalf("runSourceRetirementCleanup failed without herd.yaml: %v", err)
	}
	if report.Retired != 0 || len(report.Candidates) != 0 {
		t.Fatalf("unexpected report on empty root: %+v", report)
	}

	// 2. Malformed configuration must fail closed and never be treated as harmless absence
	rootMalformed := t.TempDir()
	herdDir := filepath.Join(rootMalformed, ".herd")
	if err := os.MkdirAll(herdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), []byte("invalid: yaml: [syntax"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errMalformed := runSourceRetirementCleanup(context.Background(), rootMalformed, true)
	if errMalformed == nil {
		t.Fatal("expected malformed herd.yaml to fail closed with error, got nil")
	}
}

func TestDrainAdaptersRetireSourceLanes_MalformedConfigFailsClosed(t *testing.T) {
	rootMalformed := t.TempDir()
	herdDir := filepath.Join(rootMalformed, ".herd")
	if err := os.MkdirAll(herdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdDir, "herd.yaml"), []byte("invalid: yaml: [syntax"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &drainAdapters{
		root:       rootMalformed,
		repository: "fixture-repo",
	}
	err := a.retireSourceLanes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "load herd configuration") {
		t.Fatalf("expected drain hook to fail closed on malformed herd.yaml, got %v", err)
	}
}
