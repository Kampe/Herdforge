package herdr

import (
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

	"github.com/Kampe/Herdforge/pkg/launch"
)

func TestNativeSourceRetirementCorruptJournalFailsClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "retirement-phases.jsonl")
	if err := os.WriteFile(p, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := &NativeSourceRetirementOp{Root: t.TempDir(), JournalPath: p}
	if _, err := op.Completed(SourceRetirementManifest{Generation: "g", CandidateSHA: strings.Repeat("a", 40), AgentName: "r", BindingDigest: "d"}); err == nil {
		t.Fatal("corrupt source retirement journal was silently ignored")
	}
}

func TestNativeSourceRetirementPositiveEndToEndPreservingSourceAndBranch(t *testing.T) {
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

	baseSHABytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))

	// Create worktree for source lane
	wtRel := ".worktrees/mender-fac794-source-retirement"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-794-source-retirement"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	// Add a commit in the worktree
	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("feature work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "feature.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "feat: implement feature").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}

	candidateSHABytes, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	candidateSHA := strings.TrimSpace(string(candidateSHABytes))

	agentName := "forge-mender-fac794-gem-6774ef2d"
	sessionID := "session-uuid-1234"

	// Write report artifact
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

	// Write launch receipt
	launchReceiptsRel := ".herd/launch-receipts.jsonl"
	launchReceiptsPath := filepath.Join(root, launchReceiptsRel)
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
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
		Repository:   "fixture-repo",
	}
	launchBytes, _ := json.Marshal(launchReceipt)
	if err := os.WriteFile(launchReceiptsPath, append(launchBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: candidateSHA, BaseSHA: baseSHA, Branch: branch,
		Worktree: wtRel, Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: sessionID, AgentName: agentName, Role: "mender", AgentKind: "opencode",
		ReportArtifact: reportRel, ReportDigest: reportDigest, Generation: "gen-1", Nonce: "nonce-1",
	})

	// Mock Herdr calls
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	tabClosed := false
	runHerdr = func(args ...string) (string, error) {
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
	}

	op := &NativeSourceRetirementOp{
		Root:               root,
		RepositoryIdentity: "fixture-repo",
	}

	// 1. Observe and evaluate
	evidence, err := op.Observe(m)
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	d := EvaluateSourceRetirement(evidence)
	if !d.Eligible {
		t.Fatalf("expected eligible, got: %+v", d)
	}

	// 2. Retire
	report, err := RetireSourceLanesContext(nil, op, []SourceRetirementManifest{m}, false)
	if err != nil {
		t.Fatalf("RetireSourceLanesContext failed: %v", err)
	}
	if report.Retired != 1 || report.Failed != 0 || report.Blocked != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	// 3. CRITICAL INVARIANT ASSERTIONS:
	// - Worktree must STILL exist on disk!
	if info, err := os.Stat(wtPath); os.IsNotExist(err) || !info.IsDir() {
		t.Fatalf("INVARIANT VIOLATED: source worktree was deleted! %s", wtPath)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "feature.txt")); err != nil {
		t.Fatalf("INVARIANT VIOLATED: source worktree files were removed! %v", err)
	}

	// - Branch must STILL exist in git!
	branchCheck, err := exec.Command("git", "-C", root, "rev-parse", "--verify", branch).Output()
	if err != nil {
		t.Fatalf("INVARIANT VIOLATED: source branch was deleted! %v", err)
	}
	if strings.TrimSpace(string(branchCheck)) != candidateSHA {
		t.Fatalf("branch tip drifted: got %s want %s", strings.TrimSpace(string(branchCheck)), candidateSHA)
	}

	// - Phase records and receipt must exist with repo-relative records
	if complete, err := op.Completed(m); !complete || err != nil {
		t.Fatalf("expected completed=true, got %t (err: %v)", complete, err)
	}

	// 4. Retry is idempotent and skips
	report2, err := RetireSourceLanesContext(nil, op, []SourceRetirementManifest{m}, false)
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if report2.Retired != 0 || len(report2.Candidates) != 1 || !report2.Candidates[0].Completed {
		t.Fatalf("expected replay to be marked completed with 0 new retirements: %+v", report2)
	}
}

func TestNativeSourceRetirementBlocksDirtyOrDriftedWorktree(t *testing.T) {
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
	baseSHABytes, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseSHABytes))

	wtRel := ".worktrees/mender-fac794-dirty"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-794-dirty"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	// Dirty file in worktree
	if err := os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write launch receipt
	launchReceiptsPath := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	launchReceipt := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         "forge-mender-dirty",
		Branch:       branch,
		Worktree:     wtRel,
		CandidateSHA: baseSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	launchBytes, _ := json.Marshal(launchReceipt)
	if err := os.WriteFile(launchReceiptsPath, append(launchBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write report artifact
	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-794\nTask: FAC-794\nAgent: forge-mender-dirty\nCandidate: " + baseSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reportData)
	reportDigest := hex.EncodeToString(sum[:])

	m := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: baseSHA, BaseSHA: baseSHA, Branch: branch,
		Worktree: wtRel, Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-dirty",
		ReportArtifact: reportRel, ReportDigest: reportDigest, Generation: "gen-dirty", Nonce: "nonce-1",
	})

	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-mender-dirty","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","focused":false,"agent_session":{"value":"session-1"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		}
		return "", errors.New("unexpected mock Herdr command")
	}

	op := &NativeSourceRetirementOp{Root: root, RepositoryIdentity: "fixture-repo"}
	evidence, err := op.Observe(m)
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	d := EvaluateSourceRetirement(evidence)
	if d.Eligible || !strings.Contains(d.Reason, "dirty") {
		t.Fatalf("expected dirty worktree refusal, got: %+v", d)
	}
}

func TestNativeSourceRetirementBlocksActiveDescendantProcesses(t *testing.T) {
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
	baseSHABytes, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(baseSHABytes))

	wtRel := ".worktrees/mender-fac794-active"
	wtPath := filepath.Join(root, wtRel)
	branch := "recovery/fac-794-active"
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	// Write launch receipt
	launchReceiptsPath := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	launchReceipt := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         "forge-mender-active",
		Branch:       branch,
		Worktree:     wtRel,
		CandidateSHA: baseSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	launchBytes, _ := json.Marshal(launchReceipt)
	if err := os.WriteFile(launchReceiptsPath, append(launchBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write report artifact
	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-794\nTask: FAC-794\nAgent: forge-mender-active\nCandidate: " + baseSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(reportData)
	reportDigest := hex.EncodeToString(sum[:])

	m := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: baseSHA, BaseSHA: baseSHA, Branch: branch,
		Worktree: wtRel, Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-active",
		ReportArtifact: reportRel, ReportDigest: reportDigest, Generation: "gen-active", Nonce: "nonce-1",
	})

	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-mender-active","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","focused":false,"agent_session":{"value":"session-1"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":12345,"name":"go","cwd":"/repo","argv":["go","test","./..."]}]}}}`, nil
		}
		return "", errors.New("unexpected mock Herdr command")
	}

	op := &NativeSourceRetirementOp{Root: root, RepositoryIdentity: "fixture-repo"}
	evidence, err := op.Observe(m)
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	d := EvaluateSourceRetirement(evidence)
	if d.Eligible || !strings.Contains(d.Reason, "active processes") {
		t.Fatalf("expected active processes refusal, got: %+v", d)
	}
}

func TestNativeSourceRetirementBlocksNonSourceRoleAndAmbiguousReceipts(t *testing.T) {
	root := t.TempDir()
	launchReceiptsPath := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Reviewer role receipt must be refused
	rReviewer := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "reviewer",
		Name:         "forge-reviewer-1",
		Branch:       "recovery/fac-794",
		Worktree:     ".worktrees/reviewer-fac794",
		CandidateSHA: strings.Repeat("a", 40),
		PaneID:       "wK:p1",
		TabID:        "wK:t1",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	b, _ := json.Marshal(rReviewer)
	if err := os.WriteFile(launchReceiptsPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	op := &NativeSourceRetirementOp{Root: root, RepositoryIdentity: "fixture-repo"}
	m := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("a", 40), Branch: "recovery/fac-794",
		Worktree: ".worktrees/reviewer-fac794", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-reviewer-1", Role: "reviewer",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64), Generation: "gen-rev", Nonce: "nonce-1",
	})
	found := op.findLaunchReceipt(m)
	if found.Accepted {
		t.Fatalf("expected empty receipt for non-source role, got %+v", found)
	}

	// 2. Ambiguous conflicting receipts for same task/name/worktree must fail closed
	r1 := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         "forge-mender-1",
		Branch:       "recovery/fac-794",
		Worktree:     ".worktrees/mender-fac794",
		CandidateSHA: strings.Repeat("a", 40),
		PaneID:       "wK:p1",
		TabID:        "wK:t1",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	r2 := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         "forge-mender-1",
		Branch:       "recovery/fac-794",
		Worktree:     ".worktrees/mender-fac794",
		CandidateSHA: strings.Repeat("b", 40), // Different candidate
		PaneID:       "wK:p2",
		TabID:        "wK:t2",
		HerdrSession: "session-2", // Different session
		Repository:   "fixture-repo",
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if err := os.WriteFile(launchReceiptsPath, append(append(b1, '\n'), append(b2, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	mConflict := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "fixture-repo", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("c", 40), Branch: "recovery/fac-794",
		Worktree: ".worktrees/mender-fac794", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-1", Role: "mender",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64), Generation: "gen-conf", Nonce: "nonce-1",
	})
	foundConf := op.findLaunchReceipt(mConflict)
	if foundConf.Accepted {
		t.Fatalf("expected empty receipt on ambiguous match, got %+v", foundConf)
	}
}

func TestCloseSettledSourceTabRejectsActiveProcessesAtCloseTime(t *testing.T) {
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })

	agent := AgentEntry{
		Name:       "forge-mender-race",
		TabID:      "wK:t1",
		PaneID:     "wK:p1",
		Workspace:  "wK",
		TerminalID: "term-1",
		Status:     "idle",
		Focused:    new(bool),
	}
	agent.Session.Value = "session-1"

	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-mender-race","agent_status":"idle","pane_id":"wK:p1","tab_id":"wK:t1","workspace_id":"wK","terminal_id":"term-1","focused":false,"agent_session":{"value":"session-1"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			// A process was started right before close
			return `{"result":{"process_info":{"pane_id":"wK:p1","shell_pid":100,"foreground_processes":[{"pid":200,"name":"make","argv":["make","test"]}]}}}`, nil
		}
		return "", errors.New("unexpected mock Herdr command")
	}

	err := CloseSettledSourceTab(agent)
	if err == nil || !strings.Contains(err.Error(), "active non-idle processes") {
		t.Fatalf("expected active processes error on close, got: %v", err)
	}
}
