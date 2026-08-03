package eligibility

import (
	"context"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// contractCase is shared across Memory and a GitHub-shaped adapter so both
// backends honor the same eligibility outcomes (FAC-123 adapter contract).
type contractCase struct {
	name      string
	seed      []*provider.Task
	facts     Facts
	claimRole string
	// wantEligible refs in claim order
	wantEligible []string
	// wantBlocked refs (any order)
	wantBlocked []string
	// wantGrooming refs
	wantGrooming []string
}

func eligibilityContractCases() []contractCase {
	return []contractCase{
		{
			name: "ready worker eligible; blocked dep held; unlabeled groomed",
			seed: []*provider.Task{
				withRisk(readyTask("FAC-3")),
				withRisk(readyTask("FAC-10")),
				func() *provider.Task {
					t := withRisk(readyTask("FAC-119"))
					return t
				}(),
				func() *provider.Task {
					t := withRisk(readyTask("FAC-50"))
					t.Labels = []string{"risk:R1"} // unlabeled role
					return t
				}(),
			},
			facts: Facts{
				Blockers: map[string][]string{"FAC-119": {"FAC-124"}},
				OpenRefs: map[string]bool{"FAC-124": true},
			},
			claimRole:    "worker",
			wantEligible: []string{"FAC-3", "FAC-10"},
			wantBlocked:  []string{"FAC-119"},
			wantGrooming: []string{"FAC-50"},
		},
		{
			name: "duplicate rejected for both adapters",
			seed: []*provider.Task{
				withRisk(readyTask("FAC-137")),
			},
			facts: Facts{
				Duplicates: map[string][]string{"FAC-137": {"FAC-132"}},
			},
			claimRole:    "worker",
			wantEligible: nil,
			wantBlocked:  []string{"FAC-137"},
		},
		{
			name: "smith role cannot claim worker-only card",
			seed: []*provider.Task{
				withRisk(readyTask("FAC-7")),
			},
			claimRole:   "forge-smith",
			wantBlocked: []string{"FAC-7"},
		},
	}
}

func TestEligibilityContract_MemoryProvider(t *testing.T) {
	for _, tc := range eligibilityContractCases() {
		t.Run("memory/"+tc.name, func(t *testing.T) {
			mp := provider.NewMemoryProvider()
			for _, tk := range tc.seed {
				c := cloneTask(tk)
				c.ProjectID = "p1"
				mp.AddTask(c)
			}
			rep, err := EvaluateBoard(context.Background(), mp, "p1", "", tc.facts, tc.claimRole)
			if err != nil {
				t.Fatal(err)
			}
			assertRefs(t, "eligible", refsOf(rep.Eligible), tc.wantEligible)
			assertRefsSet(t, "blocked", refsOf(rep.Blocked), tc.wantBlocked)
			assertRefsSet(t, "grooming", refsOf(rep.NeedsGrooming), tc.wantGrooming)
		})
	}
}

// githubShapeProvider is a non-Kaneo adapter stand-in: GitHub-like status
// spellings ("open"/"closed") and label sets, without mutating pkg/provider.
type githubShapeProvider struct {
	mu    sync.Mutex
	tasks []*provider.Task
}

func (g *githubShapeProvider) seed(tasks []*provider.Task) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tasks = nil
	for _, tk := range tasks {
		c := cloneTask(tk)
		// GitHub-style status spellings at the boundary; eligibility normalizes.
		switch c.Status {
		case "to-do":
			c.Status = "open"
		case "done":
			c.Status = "closed"
		}
		g.tasks = append(g.tasks, c)
	}
}

func (g *githubShapeProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, tk := range g.tasks {
		if tk.ID == id || tk.Ref == id {
			return cloneTask(tk), nil
		}
	}
	return nil, nil
}

func (g *githubShapeProvider) ListTasks(_ context.Context, _, status string) ([]*provider.Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*provider.Task
	for _, tk := range g.tasks {
		// Adapter-local filter using raw GitHub spelling; eligibility still
		// normalizes each task.Status independently.
		match := status == "" ||
			tk.Status == status ||
			(status == "to-do" && tk.Status == "open") ||
			(status == "done" && tk.Status == "closed")
		if match {
			out = append(out, cloneTask(tk))
		}
	}
	return out, nil
}

func (g *githubShapeProvider) ClaimTask(context.Context, string, string) error {
	return nil
}
func (g *githubShapeProvider) UpdateStatus(context.Context, string, string) error {
	return nil
}
func (g *githubShapeProvider) AddComment(context.Context, string, string) error {
	return nil
}

func TestEligibilityContract_GitHubShapedAdapter(t *testing.T) {
	gp := &githubShapeProvider{}
	for _, tc := range eligibilityContractCases() {
		t.Run("github-shape/"+tc.name, func(t *testing.T) {
			gp.seed(tc.seed)
			// statusFilter "to-do" exercises adapter mapping open↔to-do.
			rep, err := EvaluateBoard(context.Background(), gp, "", "to-do", tc.facts, tc.claimRole)
			if err != nil {
				t.Fatal(err)
			}
			assertRefs(t, "eligible", refsOf(rep.Eligible), tc.wantEligible)
			assertRefsSet(t, "blocked", refsOf(rep.Blocked), tc.wantBlocked)
			assertRefsSet(t, "grooming", refsOf(rep.NeedsGrooming), tc.wantGrooming)
		})
	}
}

func assertRefs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("%s: want empty, got %v", label, got)
		}
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d]=%s want %s (full got=%v)", label, i, got[i], want[i], got)
		}
	}
}

func assertRefsSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("%s: want empty, got %v", label, got)
		}
		return
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Fatalf("%s: missing %s in %v", label, w, got)
		}
	}
}
