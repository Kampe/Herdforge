package herdr

import (
	"crypto/ed25519"
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

func sourceRetirementManifest(t *testing.T, generation string) SourceRetirementManifest {
	t.Helper()
	m := NewSourceRetirementManifest(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), SourceRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("a", 40), Branch: "recovery/fac-794-source-retirement",
		Worktree: ".worktrees/mender-fac794-source-retirement", Workspace: "wK",
		TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1", SessionID: "session-1", SessionGeneration: "",
		AgentName: "forge-mender-fac794-gem-6774ef2d", Role: "mender", AgentKind: "opencode",
		Provider: "litellm/lazer", Model: "gemini-3.7-flash",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64),
		Generation: generation, Nonce: "nonce-1",
	})
	if err := ValidateSourceRetirementManifest(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sourceRetirementEvidence(m SourceRetirementManifest) SourceRetirementEvidence {
	launch := launch.Receipt{
		Accepted:     true,
		TaskRef:      m.TaskRef,
		Role:         m.Role,
		Name:         m.AgentName,
		Branch:       m.Branch,
		Worktree:     m.Worktree,
		CandidateSHA: m.CandidateSHA,
		PaneID:       m.PaneID,
		TabID:        m.TabID,
		HerdrSession: m.SessionID,
		Repository:   m.Repository,
	}
	handoff := SourceRetirementHandoff{
		Known:        true,
		Status:       "READY",
		CandidateSHA: m.CandidateSHA,
		TaskRef:      m.TaskRef,
		AgentName:    m.AgentName,
		ReportDigest: m.ReportDigest,
		ArtifactPath: m.ReportArtifact,
	}
	focused := false
	return SourceRetirementEvidence{
		Manifest: m,
		Launch:   launch,
		Handoff:  handoff,
		Live: SourceRetirementLive{
			Status:     "idle",
			Focused:    &focused,
			SessionID:  m.SessionID,
			TabID:      m.TabID,
			PaneID:     m.PaneID,
			TerminalID: m.TerminalID,
			Workspace:  m.Workspace,
			TabPresent: true,
		},
		Worktree: SourceRetirementWorktree{
			Known:  true,
			Dirty:  false,
			Head:   m.CandidateSHA,
			Branch: m.Branch,
		},
		Repository: m.Repository,
	}
}

func TestEvaluateSourceRetirementAllowsExactCleanSettledReadyLane(t *testing.T) {
	m := sourceRetirementManifest(t, "g1")
	e := sourceRetirementEvidence(m)
	d := EvaluateSourceRetirement(e)
	if !d.Eligible {
		t.Fatalf("expected eligible, got: %+v", d)
	}
}

func TestEvaluateSourceRetirementRequiresExactCandidateAndHandoff(t *testing.T) {
	for name, mutate := range map[string]func(*SourceRetirementEvidence){
		"missing handoff":       func(e *SourceRetirementEvidence) { e.Handoff.Known = false },
		"wrong candidate sha":   func(e *SourceRetirementEvidence) { e.Handoff.CandidateSHA = strings.Repeat("c", 40) },
		"wrong task ref":        func(e *SourceRetirementEvidence) { e.Handoff.TaskRef = "FAC-OTHER" },
		"wrong agent name":      func(e *SourceRetirementEvidence) { e.Handoff.AgentName = "other-agent" },
		"handoff working":       func(e *SourceRetirementEvidence) { e.Handoff.Status = "working" },
		"handoff blocked":       func(e *SourceRetirementEvidence) { e.Handoff.Status = "blocked" },
		"report digest mismatch": func(e *SourceRetirementEvidence) { e.Handoff.ReportDigest = strings.Repeat("e", 64) },
		"launch rejected":       func(e *SourceRetirementEvidence) { e.Launch.Accepted = false },
		"launch ref mismatch":   func(e *SourceRetirementEvidence) { e.Launch.TaskRef = "FAC-OTHER" },
		"launch branch mismatch": func(e *SourceRetirementEvidence) { e.Launch.Branch = "other-branch" },
	} {
		t.Run(name, func(t *testing.T) {
			m := sourceRetirementManifest(t, "g1")
			e := sourceRetirementEvidence(m)
			mutate(&e)
			if d := EvaluateSourceRetirement(e); d.Eligible || !strings.HasPrefix(d.Reason, "BLOCKED:") {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

func TestEvaluateSourceRetirementProtectsStandingCoordinatorCanary(t *testing.T) {
	for name, mutate := range map[string]func(*SourceRetirementEvidence){
		"standing live":      func(e *SourceRetirementEvidence) { e.Live.IsStanding = true },
		"coordinator live":   func(e *SourceRetirementEvidence) { e.Live.IsCoordinator = true },
		"coordinator name":   func(e *SourceRetirementEvidence) { e.Manifest.AgentName = "forge-orchestrator-39a9827d2b" },
		"canary live":        func(e *SourceRetirementEvidence) { e.Live.IsCanary = true },
		"canary name":        func(e *SourceRetirementEvidence) { e.Manifest.AgentName = "forge-canary-test" },
	} {
		t.Run(name, func(t *testing.T) {
			m := sourceRetirementManifest(t, "g1")
			e := sourceRetirementEvidence(m)
			mutate(&e)
			if d := EvaluateSourceRetirement(e); d.Eligible || !strings.HasPrefix(d.Reason, "BLOCKED:") {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

func TestEvaluateSourceRetirementProtectsActiveFocusedAndChangedLanes(t *testing.T) {
	for name, mutate := range map[string]func(*SourceRetirementEvidence){
		"working status":        func(e *SourceRetirementEvidence) { e.Live.Status = "working" },
		"starting status":       func(e *SourceRetirementEvidence) { e.Live.Status = "starting" },
		"focused tab":           func(e *SourceRetirementEvidence) { *e.Live.Focused = true },
		"unknown focus":         func(e *SourceRetirementEvidence) { e.Live.Focused = nil },
		"session mismatch":      func(e *SourceRetirementEvidence) { e.Live.SessionID = "other-session" },
		"tab ID mismatch":       func(e *SourceRetirementEvidence) { e.Live.TabID = "wK:t99" },
		"pane ID mismatch":      func(e *SourceRetirementEvidence) { e.Live.PaneID = "wK:p99" },
		"terminal ID mismatch":  func(e *SourceRetirementEvidence) { e.Live.TerminalID = "term-99" },
		"workspace mismatch":    func(e *SourceRetirementEvidence) { e.Live.Workspace = "wOTHER" },
		"active descendant":     func(e *SourceRetirementEvidence) { e.Live.ActiveDescendants = []string{"go test ./..."} },
	} {
		t.Run(name, func(t *testing.T) {
			m := sourceRetirementManifest(t, "g1")
			e := sourceRetirementEvidence(m)
			mutate(&e)
			if d := EvaluateSourceRetirement(e); d.Eligible || !strings.HasPrefix(d.Reason, "BLOCKED:") {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

func TestEvaluateSourceRetirementRequiresCleanUntamperedWorktree(t *testing.T) {
	for name, mutate := range map[string]func(*SourceRetirementEvidence){
		"unknown worktree":  func(e *SourceRetirementEvidence) { e.Worktree.Known = false },
		"dirty worktree":    func(e *SourceRetirementEvidence) { e.Worktree.Dirty = true },
		"head advanced":     func(e *SourceRetirementEvidence) { e.Worktree.Head = strings.Repeat("c", 40) },
		"branch changed":    func(e *SourceRetirementEvidence) { e.Worktree.Branch = "other-branch" },
		"repo mismatch":     func(e *SourceRetirementEvidence) { e.Repository = "other-repo" },
	} {
		t.Run(name, func(t *testing.T) {
			m := sourceRetirementManifest(t, "g1")
			e := sourceRetirementEvidence(m)
			mutate(&e)
			if d := EvaluateSourceRetirement(e); d.Eligible || !strings.HasPrefix(d.Reason, "BLOCKED:") {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

type sourceRetirementFake struct {
	events   []string
	evidence map[string]SourceRetirementEvidence
	fail     string
}

func (f *sourceRetirementFake) Observe(m SourceRetirementManifest) (SourceRetirementEvidence, error) {
	return f.evidence[m.Generation], nil
}
func (f *sourceRetirementFake) Revalidate(m SourceRetirementManifest, phase string) error {
	return f.step("revalidate-"+phase, m)
}
func (f *sourceRetirementFake) Journal(m SourceRetirementManifest, phase string) error {
	return f.step("journal-"+phase, m)
}
func (f *sourceRetirementFake) step(name string, m SourceRetirementManifest) error {
	f.events = append(f.events, name+":"+m.Generation)
	if f.fail == name+":"+m.Generation {
		return errors.New("injected failure on " + name)
	}
	return nil
}
func (f *sourceRetirementFake) Close(m SourceRetirementManifest) error {
	return f.step("close", m)
}
func (f *sourceRetirementFake) VerifyAbsence(m SourceRetirementManifest) error {
	return f.step("verify-absence", m)
}
func (f *sourceRetirementFake) Receipt(m SourceRetirementManifest, d SourceRetirementDecision) error {
	return f.step("receipt", m)
}

func TestRetireSourceLanesPreflightsAllBeforeMutationAndMakesBoundedProgress(t *testing.T) {
	m1 := sourceRetirementManifest(t, "g1")
	m2 := sourceRetirementManifest(t, "g2")
	m2.TaskRef = "FAC-795"
	m2.Branch = "recovery/fac-795"
	m2.Worktree = ".worktrees/mender-fac795"
	m2.TabID = "wK:t17H"
	m2.PaneID = "wK:p17H"
	m2.AgentName = "forge-mender-fac795-gem-12345678"
	m2.BindingDigest = SourceRetirementBindingDigest(m2)

	// Case 1: One lane blocked, eligible lane retires while blocked lane remains blocked
	e1 := sourceRetirementEvidence(m1)
	e2Blocked := sourceRetirementEvidence(m2)
	e2Blocked.Worktree.Dirty = true

	f := &sourceRetirementFake{
		evidence: map[string]SourceRetirementEvidence{
			"g1": e1,
			"g2": e2Blocked,
		},
	}
	r, err := RetireSourceLanes(f, []SourceRetirementManifest{m1, m2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Retired != 1 || r.Blocked != 1 {
		t.Fatalf("expected 1 retired, 1 blocked; got report=%+v events=%v", r, f.events)
	}

	// Case 2: Both eligible -> both retired in orderly fashion
	e2Eligible := sourceRetirementEvidence(m2)
	f.evidence["g2"] = e2Eligible
	f.events = nil

	r, err = RetireSourceLanes(f, []SourceRetirementManifest{m2, m1}, false)
	if err != nil || r.Retired != 2 {
		t.Fatalf("expected 2 retired; report=%+v err=%v", r, err)
	}
	wantEvents := []string{
		"revalidate-close:g1", "journal-close-intent:g1", "close:g1", "verify-absence:g1", "journal-close-done:g1", "receipt:g1", "journal-complete:g1",
		"revalidate-close:g2", "journal-close-intent:g2", "close:g2", "verify-absence:g2", "journal-close-done:g2", "receipt:g2", "journal-complete:g2",
	}
	if len(f.events) != len(wantEvents) {
		t.Fatalf("events length mismatch: got %v want %v", f.events, wantEvents)
	}
	for i := range wantEvents {
		if f.events[i] != wantEvents[i] {
			t.Fatalf("event[%d] mismatch: got %s want %s", i, f.events[i], wantEvents[i])
		}
	}

	// Case 3: Bounded progress: one lane mutation failure does NOT abort other eligible lanes
	f.events = nil
	f.fail = "close:g1"
	r, err = RetireSourceLanes(f, []SourceRetirementManifest{m1, m2}, false)
	if err == nil {
		t.Fatal("expected error on failed close:g1")
	}
	if r.Retired != 1 || r.Failed != 1 {
		t.Fatalf("expected 1 retired, 1 failed; report=%+v", r)
	}
	// g2 should still have proceeded and completed!
	g2Found := false
	for _, ev := range f.events {
		if ev == "journal-complete:g2" {
			g2Found = true
			break
		}
	}
	if !g2Found {
		t.Fatalf("g2 should have completed despite g1 failure; events=%v", f.events)
	}
}

func TestSourceRetirementManifestRegistry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifests.jsonl")
	reg := SourceRetirementRegistry{Path: p}
	m := sourceRetirementManifest(t, "g1")

	if err := reg.Record(m); err != nil {
		t.Fatal(err)
	}
	if err := reg.Record(m); err != nil {
		t.Fatal(err)
	}

	rows, err := reg.Latest()
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (err: %v)", len(rows), err)
	}
	if rows[0].Generation != "g1" || rows[0].CandidateSHA != m.CandidateSHA {
		t.Fatalf("row mismatch: %+v", rows[0])
	}
}

func TestParseStructuredHandoffReport_ValidAndInvalid(t *testing.T) {
	sha := strings.Repeat("a", 40)
	tests := []struct {
		name    string
		content string
		wantErr bool
		want    StructuredHandoffReport
	}{
		{
			name:    "valid structured report with key value pairs",
			content: "# Handoff Report\n\nTask: FAC-794\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: false,
			want: StructuredHandoffReport{
				TaskRef:      "FAC-794",
				AgentName:    "forge-mender-1",
				CandidateSHA: sha,
				Status:       "READY",
			},
		},
		{
			name:    "valid markdown list format",
			content: "- Task: FAC-794\n- Agent: forge-mender-1\n- Candidate: " + sha + "\n- Status: READY\n",
			wantErr: false,
			want: StructuredHandoffReport{
				TaskRef:      "FAC-794",
				AgentName:    "forge-mender-1",
				CandidateSHA: sha,
				Status:       "READY",
			},
		},
		{
			name:    "non ready status",
			content: "Task: FAC-794\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: BLOCKED\n",
			wantErr: true,
		},
		{
			name:    "prose only without structured status",
			content: "I finished the work and everything is ready for review.",
			wantErr: true,
		},
		{
			name:    "missing candidate",
			content: "Task: FAC-794\nAgent: forge-mender-1\nStatus: READY\n",
			wantErr: true,
		},
		{
			name:    "missing agent",
			content: "Task: FAC-794\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: true,
		},
		{
			name:    "missing task",
			content: "Agent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: true,
		},
		{
			name:    "conflicting duplicate status",
			content: "Task: FAC-794\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\nStatus: BLOCKED\n",
			wantErr: true,
		},
		{
			name:    "conflicting duplicate task",
			content: "Task: FAC-794\nTask: FAC-795\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: true,
		},
		{
			name:    "conflicting duplicate agent",
			content: "Task: FAC-794\nAgent: forge-mender-1\nAgent: forge-mender-2\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: true,
		},
		{
			name:    "quoted prose lines ignored",
			content: "> Task: FAC-999\n> Agent: evil-agent\n> Candidate: 0000000000000000000000000000000000000000\n> Status: READY\nTask: FAC-794\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: false,
			want: StructuredHandoffReport{
				TaskRef:      "FAC-794",
				AgentName:    "forge-mender-1",
				CandidateSHA: sha,
				Status:       "READY",
			},
		},
		{
			name:    "code block lines ignored",
			content: "```markdown\nTask: FAC-999\nAgent: evil-agent\nCandidate: 0000000000000000000000000000000000000000\nStatus: READY\n```\nTask: FAC-794\nAgent: forge-mender-1\nCandidate: " + sha + "\nStatus: READY\n",
			wantErr: false,
			want: StructuredHandoffReport{
				TaskRef:      "FAC-794",
				AgentName:    "forge-mender-1",
				CandidateSHA: sha,
				Status:       "READY",
			},
		},
		{
			name:    "short candidate sha",
			content: "Task: FAC-794\nAgent: forge-mender-1\nCandidate: abc123\nStatus: READY\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStructuredHandoffReport([]byte(tt.content))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr %v (got %+v)", err, tt.wantErr, got)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEnrollReadySourceManifests_DryRunReadOnly(t *testing.T) {
	root := t.TempDir()
	candidateSHA := strings.Repeat("a", 40)
	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("Task: FAC-794\nAgent: forge-mender-1\nCandidate: " + candidateSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}

	wtDir := filepath.Join(root, ".worktrees", "mender-fac794")
	if err := os.MkdirAll(wtDir, 0o700); err != nil {
		t.Fatal(err)
	}

	launchReceiptsRel := ".herd/launch-receipts.jsonl"
	launchReceiptsPath := filepath.Join(root, launchReceiptsRel)
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lr := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         "forge-mender-1",
		Branch:       "recovery/fac-794",
		Worktree:     ".worktrees/mender-fac794",
		CandidateSHA: candidateSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: "session-1",
	}
	lrBytes, _ := json.Marshal(lr)
	if err := os.WriteFile(launchReceiptsPath, append(lrBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestsPath := SourceRetirementRegistryPath(root)

	// 1. Dry run (persist = false): returns enrolled manifests in memory without creating file
	manifests, err := EnrollReadySourceManifests(root, "fixture-repo", false)
	if err != nil {
		t.Fatalf("EnrollReadySourceManifests(dryRun) failed: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest in memory, got %d", len(manifests))
	}
	if _, err := os.Stat(manifestsPath); !os.IsNotExist(err) {
		t.Fatalf("DRY RUN VIOLATION: registry file was written to disk during dry run!")
	}

	// 2. Act (persist = true): writes file to disk
	manifests2, err := EnrollReadySourceManifests(root, "fixture-repo", true)
	if err != nil {
		t.Fatalf("EnrollReadySourceManifests(persist) failed: %v", err)
	}
	if len(manifests2) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests2))
	}
	if _, err := os.Stat(manifestsPath); err != nil {
		t.Fatalf("expected registry file to exist after persist=true: %v", err)
	}
}

func TestEnrollReadySourceManifests_CorruptRegistryFailsClosed(t *testing.T) {
	root := t.TempDir()
	manifestsPath := SourceRetirementRegistryPath(root)
	if err := os.MkdirAll(filepath.Dir(manifestsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestsPath, []byte("{invalid-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := EnrollReadySourceManifests(root, "fixture-repo", true)
	if err == nil {
		t.Fatal("expected failure on corrupt registry file, but got nil")
	}
}

type mixedBatchFake struct {
	events []string
}

func (f *mixedBatchFake) Observe(m SourceRetirementManifest) (SourceRetirementEvidence, error) {
	if m.Generation == "g-fail-obs" {
		return SourceRetirementEvidence{}, errors.New("simulated observation transport failure")
	}
	e := sourceRetirementEvidence(m)
	if m.Generation == "g-blocked" {
		e.Worktree.Dirty = true
	}
	return e, nil
}
func (f *mixedBatchFake) Revalidate(m SourceRetirementManifest, phase string) error {
	f.events = append(f.events, "revalidate:"+m.Generation)
	return nil
}
func (f *mixedBatchFake) Journal(m SourceRetirementManifest, phase string) error {
	f.events = append(f.events, "journal-"+phase+":"+m.Generation)
	return nil
}
func (f *mixedBatchFake) Close(m SourceRetirementManifest) error {
	f.events = append(f.events, "close:"+m.Generation)
	return nil
}
func (f *mixedBatchFake) VerifyAbsence(m SourceRetirementManifest) error {
	f.events = append(f.events, "verify-absence:"+m.Generation)
	return nil
}
func (f *mixedBatchFake) Receipt(m SourceRetirementManifest, d SourceRetirementDecision) error {
	f.events = append(f.events, "receipt:"+m.Generation)
	return nil
}

func TestRetireSourceLanesContext_MixedBatchFailClosedIsolation(t *testing.T) {
	mEligible := sourceRetirementManifest(t, "g-eligible")
	mFailObs := sourceRetirementManifest(t, "g-fail-obs")
	mBlocked := sourceRetirementManifest(t, "g-blocked")

	f := &mixedBatchFake{}
	report, err := RetireSourceLanesContext(nil, f, []SourceRetirementManifest{mEligible, mFailObs, mBlocked}, false)

	// 1. Error must be non-zero and aggregated
	if err == nil {
		t.Fatal("expected aggregated non-zero error on mixed batch with observation failure, got nil")
	}
	if !strings.Contains(err.Error(), "failure") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// 2. Report counts: exactly 1 retired, 1 blocked, 1 failed
	if report.Retired != 1 || report.Blocked != 1 || report.Failed != 1 {
		t.Fatalf("expected 1 retired, 1 blocked, 1 failed; got %+v", report)
	}

	// 3. Eligible lane completed all lifecycle phases
	wantEvents := []string{
		"revalidate:g-eligible",
		"journal-close-intent:g-eligible",
		"close:g-eligible",
		"verify-absence:g-eligible",
		"journal-close-done:g-eligible",
		"receipt:g-eligible",
		"journal-complete:g-eligible",
	}
	if len(f.events) != len(wantEvents) {
		t.Fatalf("unexpected events: got %v, want %v", f.events, wantEvents)
	}
	for i := range wantEvents {
		if f.events[i] != wantEvents[i] {
			t.Fatalf("event[%d] got %s want %s", i, f.events[i], wantEvents[i])
		}
	}
}

func TestEvaluateSourceRetirementAncestryAuthentication(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base").CombinedOutput(); err != nil {
		t.Fatalf("base commit: %v (%s)", err, out)
	}
	baseBytes, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseBytes))

	// Create a descendant commit
	if out, err := exec.Command("git", "-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "descendant").CombinedOutput(); err != nil {
		t.Fatalf("descendant commit: %v (%s)", err, out)
	}
	descendantBytes, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	descendantSHA := strings.TrimSpace(string(descendantBytes))

	// Create an unrelated orphan commit
	if out, err := exec.Command("git", "-C", repo, "checkout", "-q", "--orphan", "unrelated").CombinedOutput(); err != nil {
		t.Fatalf("orphan checkout: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "unrelated").CombinedOutput(); err != nil {
		t.Fatalf("orphan commit: %v (%s)", err, out)
	}
	unrelatedBytes, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedSHA := strings.TrimSpace(string(unrelatedBytes))

	// 1. Positive: launch start pin is baseSHA, manifest candidate is descendantSHA, worktree points to repo.
	mPos := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: descendantSHA, BaseSHA: baseSHA, Branch: "main",
		Worktree: ".worktrees/mender-fac794-source-retirement", Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-fac794-gem-6774ef2d", Role: "mender", AgentKind: "opencode",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64),
		Generation: "g-pos", Nonce: "nonce-1",
	})
	ePos := sourceRetirementEvidence(mPos)
	ePos.Launch.CandidateSHA = baseSHA // Start pin is base
	ePos.Worktree.Path = repo
	ePos.Worktree.Head = descendantSHA
	ePos.Handoff.CandidateSHA = descendantSHA
	dPos := EvaluateSourceRetirement(ePos)
	if !dPos.Eligible {
		t.Fatalf("expected legitimate descendant to be eligible, got: %+v", dPos)
	}

	// 2. Negative: launch start pin is unrelatedSHA (not an ancestor of descendantSHA).
	mNeg := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: descendantSHA, BaseSHA: unrelatedSHA, Branch: "main",
		Worktree: ".worktrees/mender-fac794-source-retirement", Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-fac794-gem-6774ef2d", Role: "mender", AgentKind: "opencode",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64),
		Generation: "g-neg", Nonce: "nonce-1",
	})
	eNeg := sourceRetirementEvidence(mNeg)
	eNeg.Launch.CandidateSHA = unrelatedSHA // Unrelated start pin
	eNeg.Worktree.Path = repo
	eNeg.Worktree.Head = descendantSHA
	eNeg.Handoff.CandidateSHA = descendantSHA
	dNeg := EvaluateSourceRetirement(eNeg)
	if dNeg.Eligible || !strings.Contains(dNeg.Reason, "is not an ancestor") {
		t.Fatalf("expected unrelated start pin to be blocked with ancestry error, got: %+v", dNeg)
	}

	// 3. Negative: launch candidate matches candidate, but manifest BaseSHA is unrelatedSHA.
	mNegBase := NewSourceRetirementManifest(time.Now(), SourceRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-794", TaskID: "task-794",
		CandidateSHA: descendantSHA, BaseSHA: unrelatedSHA, Branch: "main",
		Worktree: ".worktrees/mender-fac794-source-retirement", Workspace: "wK", TabID: "wK:t17G", PaneID: "wK:p17G", TerminalID: "term-1",
		SessionID: "session-1", AgentName: "forge-mender-fac794-gem-6774ef2d", Role: "mender", AgentKind: "opencode",
		ReportArtifact: ".herd/reports/fac-794.md", ReportDigest: strings.Repeat("d", 64),
		Generation: "g-neg-base", Nonce: "nonce-1",
	})
	eNegBase := sourceRetirementEvidence(mNegBase)
	eNegBase.Launch.CandidateSHA = descendantSHA
	eNegBase.Worktree.Path = repo
	eNegBase.Worktree.Head = descendantSHA
	eNegBase.Handoff.CandidateSHA = descendantSHA
	dNegBase := EvaluateSourceRetirement(eNegBase)
	if dNegBase.Eligible || !strings.Contains(dNegBase.Reason, "is not an ancestor") {
		t.Fatalf("expected unrelated base SHA to be blocked with ancestry error, got: %+v", dNegBase)
	}
}

func TestEnrollReadySourceManifestsAncestryAuthentication(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base").CombinedOutput(); err != nil {
		t.Fatalf("base commit: %v (%s)", err, out)
	}
	baseBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseBytes))

	// Create descendant commit
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "descendant").CombinedOutput(); err != nil {
		t.Fatalf("descendant commit: %v (%s)", err, out)
	}
	descendantBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	descendantSHA := strings.TrimSpace(string(descendantBytes))

	// Create unrelated orphan commit
	if out, err := exec.Command("git", "-C", root, "checkout", "-q", "--orphan", "unrelated").CombinedOutput(); err != nil {
		t.Fatalf("orphan checkout: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "unrelated").CombinedOutput(); err != nil {
		t.Fatalf("orphan commit: %v (%s)", err, out)
	}
	unrelatedBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedSHA := strings.TrimSpace(string(unrelatedBytes))

	// Switch back to main
	if out, err := exec.Command("git", "-C", root, "checkout", "-q", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v (%s)", err, out)
	}

	agentName := "forge-mender-fac794-gem-6774ef2d"
	reportRel := ".herd/reports/fac-794.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-794\nTask: FAC-794\nAgent: " + agentName + "\nCandidate: " + descendantSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}

	wtRel := "worktrees/mender-wt"
	if err := os.MkdirAll(filepath.Join(root, wtRel), 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Negative: Launch receipt with unrelated candidate SHA (not an ancestor of report's candidate)
	launchReceiptsPath := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lrNeg := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         agentName,
		Branch:       "main",
		Worktree:     wtRel,
		CandidateSHA: unrelatedSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	lrNegBytes, _ := json.Marshal(lrNeg)
	if err := os.WriteFile(launchReceiptsPath, append(lrNegBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	enrolledNeg, err := EnrollReadySourceManifests(root, "fixture-repo", false)
	if err != nil {
		t.Fatalf("EnrollReadySourceManifests failed: %v", err)
	}
	if len(enrolledNeg) != 0 {
		t.Fatalf("expected unrelated launch pin not to be enrolled, got %d manifests: %+v", len(enrolledNeg), enrolledNeg)
	}

	// 2. Positive: Launch receipt with baseSHA (legitimate ancestor of report's candidate)
	lrPos := launch.Receipt{
		Accepted:     true,
		TaskRef:      "FAC-794",
		Role:         "mender",
		Name:         agentName,
		Branch:       "main",
		Worktree:     wtRel,
		CandidateSHA: baseSHA,
		PaneID:       "wK:p17G",
		TabID:        "wK:t17G",
		HerdrSession: "session-1",
		Repository:   "fixture-repo",
	}
	lrPosBytes, _ := json.Marshal(lrPos)
	if err := os.WriteFile(launchReceiptsPath, append(lrPosBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	enrolledPos, err := EnrollReadySourceManifests(root, "fixture-repo", false)
	if err != nil {
		t.Fatalf("EnrollReadySourceManifests failed: %v", err)
	}
	if len(enrolledPos) != 1 {
		t.Fatalf("expected legitimate descendant to be enrolled, got %d manifests: %+v", len(enrolledPos), enrolledPos)
	}
	if enrolledPos[0].BaseSHA != baseSHA || enrolledPos[0].CandidateSHA != descendantSHA {
		t.Fatalf("enrolled manifest base/candidate mismatch: got base=%s candidate=%s, want base=%s candidate=%s",
			enrolledPos[0].BaseSHA, enrolledPos[0].CandidateSHA, baseSHA, descendantSHA)
	}
}

func TestEnrollReadySourceManifests_ResolvesNativeLaunchReceiptWithoutWorktreeOrSession(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base").CombinedOutput(); err != nil {
		t.Fatalf("base commit: %v (%s)", err, out)
	}
	baseBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseBytes))

	agentName := "forge-mender-fac786-nat-b5e8985e"
	laneName := "mender-fac786-native-endpoint"
	wtRel := ".worktrees/mender-fac786-native-endpoint"
	wtAbs := filepath.Join(root, wtRel)
	if err := os.MkdirAll(wtAbs, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create branch in worktree
	branch := "recovery/fac-786-native-endpoint"
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", branch, wtAbs, "main").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	// Write work commit
	if err := os.WriteFile(filepath.Join(wtAbs, "fix.txt"), []byte("native endpoint fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtAbs, "add", "fix.txt").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command("git", "-C", wtAbs, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "-m", "fix: native endpoint").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	candidateBytes, err := exec.Command("git", "-C", wtAbs, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	candidateSHA := strings.TrimSpace(string(candidateBytes))

	// Generate receipt signing key and write published verification key to .herd/receipt.pub
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	herdDir := filepath.Join(root, ".herd")
	if err := os.MkdirAll(herdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(herdDir, "receipt.pub"), []byte(hex.EncodeToString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write TASK-CONTEXT.json in worktree (authenticates task ref, role, and session)
	unsignedTC := signedSourceTaskContext{
		ProviderType:    "kaneo",
		ProjectID:       "proj-1",
		Repository:      "fixture-repo",
		Role:            "mender",
		TaskRef:         "FAC-786",
		TaskID:          "task-fac-786",
		Branch:          branch,
		BaseSHA:         baseSHA,
		LeaseID:         "lease-1",
		LeaseGeneration: 1,
		LeaseTaskRef:    "FAC-786",
		SessionID:       "session-live-nat-1234",
		AllowedOps:      []string{"get", "list", "comment"},
		ExpiresAt:       time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	canonicalBytes, err := json.Marshal(unsignedTC)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, canonicalBytes)
	unsignedTC.Signature = hex.EncodeToString(sig)

	taskContextData, err := json.Marshal(unsignedTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtAbs, "TASK-CONTEXT.json"), taskContextData, 0o600); err != nil {
		t.Fatal(err)
	}

	// Write durable handoff report in .herd/reports/
	reportRel := ".herd/reports/fac-786.md"
	reportPath := filepath.Join(root, reportRel)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reportData := []byte("## Report for FAC-786\nTask: FAC-786\nAgent: " + agentName + "\nCandidate: " + candidateSHA + "\nStatus: READY\n")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		t.Fatal(err)
	}

	// Production launch receipt produced by herd up / recordResolvedLaunchReceipt:
	// Notice: TaskRef is the lane name "mender-fac786-native-endpoint"
	// Worktree is empty
	// HerdrSession is empty
	// CandidateSHA is empty
	// CWD is set
	launchReceiptsPath := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(launchReceiptsPath), 0o700); err != nil {
		t.Fatal(err)
	}
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
		Repository:     "fixture-repo",
		BuilderFamily:  "openai",
		Branch:         branch,
		CWD:            wtAbs,
	}
	lrBytes, _ := json.Marshal(lr)
	if err := os.WriteFile(launchReceiptsPath, append(lrBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Mock runHerdr for live AgentList resolving session
	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"` + agentName + `","agent_status":"idle","pane_id":"wK:p17G","tab_id":"wK:t17G","workspace_id":"wK","terminal_id":"term-1","cwd":"` + wtAbs + `","focused":false,"agent_session":{"value":"session-live-nat-1234"}}]}}`, nil
		}
		return "", errors.New("unsupported mock Herdr command")
	}

	manifests, err := EnrollReadySourceManifests(root, "fixture-repo", true)
	if err != nil {
		t.Fatalf("EnrollReadySourceManifests failed: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 enrolled manifest, got %d", len(manifests))
	}
	m := manifests[0]
	if m.TaskRef != "FAC-786" {
		t.Errorf("manifest TaskRef = %q, want %q", m.TaskRef, "FAC-786")
	}
	if m.Worktree != wtRel {
		t.Errorf("manifest Worktree = %q, want %q", m.Worktree, wtRel)
	}
	if m.SessionID != "session-live-nat-1234" {
		t.Errorf("manifest SessionID = %q, want %q", m.SessionID, "session-live-nat-1234")
	}
	if m.CandidateSHA != candidateSHA {
		t.Errorf("manifest CandidateSHA = %q, want %q", m.CandidateSHA, candidateSHA)
	}
}
