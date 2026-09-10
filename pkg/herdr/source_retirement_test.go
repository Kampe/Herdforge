package herdr

import (
	"errors"
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
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "recovery/fac-794-source-retirement",
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

	// Case 1: One lane blocked, preflight blocks everything before mutation
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
	if r.Retired != 0 || r.Blocked != 1 || len(f.events) != 0 {
		t.Fatalf("expected 0 retired, 1 blocked, 0 mutations; got report=%+v events=%v", r, f.events)
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
