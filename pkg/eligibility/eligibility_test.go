package eligibility

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// readyTask is a fully groomed to-do card that should be ELIGIBLE for worker
// when no external blockers/duplicates apply.
func readyTask(ref string) *provider.Task {
	return &provider.Task{
		ID:          ref,
		Ref:         ref,
		Title:       "Ready " + ref,
		Description: "## Outcome\nShip it.\n\n## Acceptance criteria\n- [ ] works\n",
		Status:      "to-do",
		Priority:    provider.PriorityHigh,
		ProjectID:   "p1",
		Labels:      []string{"worker"},
	}
}

func withRisk(t *provider.Task) *provider.Task {
	t.Labels = append(append([]string{}, t.Labels...), "risk:R1")
	return t
}

func TestEvaluateTask_TableGates(t *testing.T) {
	tests := []struct {
		name      string
		task      *provider.Task
		facts     Facts
		claimRole string
		wantState State
		wantReasonSubstr string
		// nonVacuous: after a single mutation, state must flip to ELIGIBLE
		// (proves the gate is causal, not vacuously always-blocked).
		nonVacuous func(task *provider.Task, facts Facts) (task2 *provider.Task, facts2 Facts)
	}{
		{
			name:      "eligible when fully groomed",
			task:      withRisk(readyTask("FAC-10")),
			claimRole: "worker",
			wantState: StateEligible,
		},
		{
			name:      "empty description needs grooming",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-11")); t.Description = ""; return t }(),
			claimRole: "worker",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "description:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				task.Description = "## Acceptance criteria\n- [ ] ok\n"
				return task, facts
			},
		},
		{
			name:      "missing acceptance needs grooming",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-12")); t.Description = "just a blurb, no gates"; return t }(),
			claimRole: "worker",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "acceptance:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				task.Description = "Outcome\n\n## Acceptance criteria\n- [ ] done\n"
				return task, facts
			},
		},
		{
			name:      "unlabeled needs grooming (no worker/smith fallback)",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-13")); t.Labels = []string{"risk:R1"}; return t }(),
			claimRole: "worker",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "role:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				task.Labels = []string{"worker", "risk:R1"}
				return task, facts
			},
		},
		{
			name:      "unlabeled does not match smith either",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-14")); t.Labels = []string{"risk:R2"}; return t }(),
			claimRole: "forge-smith",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "role:",
		},
		{
			name:      "role mismatch blocked (worker card, smith claim)",
			task:      withRisk(readyTask("FAC-15")),
			claimRole: "forge-smith",
			wantState: StateBlocked,
			wantReasonSubstr: "role_mismatch:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				return task, facts // flip claim role in dedicated check below
			},
		},
		{
			name: "blocked dependency held back",
			task: withRisk(readyTask("FAC-119")),
			facts: Facts{
				Blockers: map[string][]string{"FAC-119": {"FAC-124"}},
				OpenRefs: map[string]bool{"FAC-124": true},
			},
			claimRole: "worker",
			wantState: StateBlocked,
			wantReasonSubstr: "dependency:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				// Resolve blocker: remove from open set → becomes eligible.
				facts.OpenRefs = map[string]bool{}
				return task, facts
			},
		},
		{
			name: "duplicate rejected",
			task: withRisk(readyTask("FAC-137")),
			facts: Facts{
				Duplicates: map[string][]string{"FAC-137": {"FAC-132"}},
			},
			claimRole: "worker",
			wantState: StateBlocked,
			wantReasonSubstr: "duplicate:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				facts.Duplicates = nil
				return task, facts
			},
		},
		{
			name: "already integrated",
			task: withRisk(readyTask("FAC-20")),
			facts: Facts{
				Integrated: map[string]string{"FAC-20": "main@abc123 task-bound receipt"},
			},
			claimRole: "worker",
			wantState: StateAlreadyDone,
			wantReasonSubstr: "integrated:",
		},
		{
			name:      "done status already done",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-21")); t.Status = "done"; return t }(),
			claimRole: "worker",
			wantState: StateAlreadyDone,
			wantReasonSubstr: "status: already done",
		},
		{
			name:      "missing priority needs grooming",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-22")); t.Priority = ""; return t }(),
			claimRole: "worker",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "priority:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				task.Priority = provider.PriorityMedium
				return task, facts
			},
		},
		{
			name:      "missing risk needs grooming",
			task:      readyTask("FAC-23"), // no risk label
			claimRole: "worker",
			wantState: StateNeedsGrooming,
			wantReasonSubstr: "risk:",
			nonVacuous: func(task *provider.Task, facts Facts) (*provider.Task, Facts) {
				facts.RiskHints = map[string]string{"FAC-23": "R2"}
				return task, facts
			},
		},
		{
			name:      "explicit role_map allows unlabeled board labels",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-24")); t.Labels = []string{"risk:R0"}; return t }(),
			facts:     Facts{RoleMap: map[string]string{"FAC-24": "worker"}},
			claimRole: "worker",
			wantState: StateEligible,
		},
		{
			name:      "herd-smith alias matches forge-smith claim",
			task:      func() *provider.Task { t := withRisk(readyTask("FAC-25")); t.Labels = []string{"herd-smith", "risk:R1"}; return t }(),
			claimRole: "forge-smith",
			wantState: StateEligible,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clone task lightly so nonVacuous mutations stay per-subtest.
			task := cloneTask(tt.task)
			got := EvaluateTask(task, tt.facts, tt.claimRole)
			if got.State != tt.wantState {
				t.Fatalf("state = %s, want %s; reasons=%v", got.State, tt.wantState, got.Reasons)
			}
			if tt.wantReasonSubstr != "" && !reasonHas(got.Reasons, tt.wantReasonSubstr) {
				t.Fatalf("reasons %v missing substr %q", got.Reasons, tt.wantReasonSubstr)
			}
			if tt.wantState == StateEligible && len(got.Reasons) != 0 {
				t.Fatalf("ELIGIBLE must have empty reasons, got %v", got.Reasons)
			}

			if tt.nonVacuous != nil && tt.wantState != StateEligible {
				// Role-mismatch case: flip claim role rather than task.
				if strings.Contains(tt.name, "role mismatch") {
					fixed := EvaluateTask(task, tt.facts, "worker")
					if fixed.State != StateEligible {
						t.Fatalf("non-vacuous: claiming as worker must be ELIGIBLE, got %s %v", fixed.State, fixed.Reasons)
					}
					return
				}
				t2, f2 := tt.nonVacuous(cloneTask(task), cloneFacts(tt.facts))
				fixed := EvaluateTask(t2, f2, tt.claimRole)
				if fixed.State != StateEligible {
					t.Fatalf("non-vacuous mutation must yield ELIGIBLE, got %s %v", fixed.State, fixed.Reasons)
				}
			}
		})
	}
}

func TestSelectEligible_SortPriorityThenRef(t *testing.T) {
	mp := provider.NewMemoryProvider()
	// Same priority: FAC-3 before FAC-10 (numeric). Higher priority first.
	for _, tk := range []*provider.Task{
		withRisk(func() *provider.Task {
			t := readyTask("FAC-9")
			t.Priority = provider.PriorityMedium
			return t
		}()),
		withRisk(func() *provider.Task {
			t := readyTask("FAC-3")
			t.Priority = provider.PriorityUrgent
			return t
		}()),
		withRisk(func() *provider.Task {
			t := readyTask("FAC-10")
			t.Priority = provider.PriorityUrgent
			return t
		}()),
	} {
		mp.AddTask(tk)
	}

	got, err := SelectEligible(context.Background(), mp, "p1", Facts{}, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 eligible, got %d %#v", len(got), got)
	}
	want := []string{"FAC-3", "FAC-10", "FAC-9"}
	for i, w := range want {
		if got[i].Ref != w {
			t.Fatalf("rank[%d]=%s want %s (full=%v)", i, got[i].Ref, w, refsOf(got))
		}
	}
}

func TestSelectEligible_ProviderErrorNotEmptyList(t *testing.T) {
	boom := errors.New("provider transport down")
	tp := &errProvider{err: boom}
	got, err := SelectEligible(context.Background(), tp, "p1", Facts{}, "worker")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Fatalf("error posture must not yield candidates, got %v", got)
	}
	if !errors.Is(err, boom) && !strings.Contains(err.Error(), "provider transport down") {
		t.Fatalf("error must wrap provider failure, got %v", err)
	}
}

func TestSelectEligible_RequiresClaimRole(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(withRisk(readyTask("FAC-1")))
	_, err := SelectEligible(context.Background(), mp, "p1", Facts{}, "")
	if err == nil {
		t.Fatal("empty claim role must error")
	}
}

func TestEvaluateBoard_Buckets(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(withRisk(readyTask("FAC-1")))
	blocked := withRisk(readyTask("FAC-2"))
	mp.AddTask(blocked)
	groom := withRisk(readyTask("FAC-3"))
	groom.Description = ""
	mp.AddTask(groom)
	done := withRisk(readyTask("FAC-4"))
	done.Status = "done"
	mp.AddTask(done)

	facts := Facts{
		Blockers: map[string][]string{"FAC-2": {"FAC-99"}},
		OpenRefs: map[string]bool{"FAC-99": true},
	}
	rep, err := EvaluateBoard(context.Background(), mp, "p1", "", facts, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Eligible) != 1 || rep.Eligible[0].Ref != "FAC-1" {
		t.Fatalf("eligible=%v", refsOf(rep.Eligible))
	}
	if len(rep.Blocked) != 1 || rep.Blocked[0].Ref != "FAC-2" {
		t.Fatalf("blocked=%v", refsOf(rep.Blocked))
	}
	if len(rep.NeedsGrooming) != 1 || rep.NeedsGrooming[0].Ref != "FAC-3" {
		t.Fatalf("grooming=%v", refsOf(rep.NeedsGrooming))
	}
	if len(rep.AlreadyDone) != 1 || rep.AlreadyDone[0].Ref != "FAC-4" {
		t.Fatalf("done=%v", refsOf(rep.AlreadyDone))
	}
}

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"To Do":        "to-do",
		"todo":         "to-do",
		"In Progress":  "in-progress",
		"review":       "in-review",
		"closed":       "done",
		"":             "unknown",
		"weird-status": "unknown:weird-status",
	}
	for in, want := range cases {
		if got := NormalizeStatus(in); got != want {
			t.Errorf("NormalizeStatus(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHasAcceptanceCriteria(t *testing.T) {
	if HasAcceptanceCriteria("") {
		t.Fatal("empty must fail")
	}
	if HasAcceptanceCriteria("no markers here") {
		t.Fatal("blurb without markers must fail")
	}
	if !HasAcceptanceCriteria("## Acceptance criteria\n- ship it") {
		t.Fatal("section header must pass")
	}
	if !HasAcceptanceCriteria("checklist:\n- [ ] one\n") {
		t.Fatal("checkbox must pass")
	}
}

// --- helpers ---

type errProvider struct{ err error }

func (e *errProvider) GetTask(context.Context, string) (*provider.Task, error) {
	return nil, e.err
}
func (e *errProvider) ListTasks(context.Context, string, string) ([]*provider.Task, error) {
	return nil, e.err
}
func (e *errProvider) ClaimTask(context.Context, string, string) error { return e.err }
func (e *errProvider) UpdateStatus(context.Context, string, string) error {
	return e.err
}
func (e *errProvider) AddComment(context.Context, string, string) error { return e.err }

func cloneTask(t *provider.Task) *provider.Task {
	if t == nil {
		return nil
	}
	c := *t
	if t.Labels != nil {
		c.Labels = append([]string(nil), t.Labels...)
	}
	return &c
}

func cloneFacts(f Facts) Facts {
	out := Facts{}
	if f.Blockers != nil {
		out.Blockers = map[string][]string{}
		for k, v := range f.Blockers {
			out.Blockers[k] = append([]string(nil), v...)
		}
	}
	if f.OpenRefs != nil {
		out.OpenRefs = map[string]bool{}
		for k, v := range f.OpenRefs {
			out.OpenRefs[k] = v
		}
	}
	if f.Duplicates != nil {
		out.Duplicates = map[string][]string{}
		for k, v := range f.Duplicates {
			out.Duplicates[k] = append([]string(nil), v...)
		}
	}
	if f.Integrated != nil {
		out.Integrated = map[string]string{}
		for k, v := range f.Integrated {
			out.Integrated[k] = v
		}
	}
	if f.RoleMap != nil {
		out.RoleMap = map[string]string{}
		for k, v := range f.RoleMap {
			out.RoleMap[k] = v
		}
	}
	if f.RiskHints != nil {
		out.RiskHints = map[string]string{}
		for k, v := range f.RiskHints {
			out.RiskHints[k] = v
		}
	}
	return out
}

func reasonHas(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func refsOf(rows []Result) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Ref
	}
	return out
}
