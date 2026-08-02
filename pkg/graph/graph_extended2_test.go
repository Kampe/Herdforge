package graph

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestComputeBlastRadius_Transitive(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "lib-core", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "lib-utils", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "svc-api", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "svc-web", DefaultBranch: "main"})

	wg.AddDependency("lib-utils", "lib-core", "import")
	wg.AddDependency("svc-api", "lib-utils", "import")
	wg.AddDependency("svc-web", "svc-api", "import")

	affected, err := wg.ComputeBlastRadius(context.Background(), "lib-core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"lib-utils", "svc-api", "svc-web"}
	if len(affected) != 3 {
		t.Fatalf("expected 3 transitive affected, got %d: %v", len(affected), affected)
	}
	for i, name := range expected {
		if affected[i] != name {
			t.Errorf("expected affected[%d]=%s, got %s", i, name, affected[i])
		}
	}
}

func TestComputeBlastRadius_NoDownstream(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "leaf", DefaultBranch: "main"})

	affected, err := wg.ComputeBlastRadius(context.Background(), "leaf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("expected 0 affected for leaf project, got %d", len(affected))
	}
}

func TestComputeBlastRadius_EmptyGraph(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "orphan", DefaultBranch: "main"})

	affected, err := wg.ComputeBlastRadius(context.Background(), "orphan")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("expected 0 affected for orphan, got %d", len(affected))
	}
}

func TestAddDependency_Dedup(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddDependency("a", "b", "import")
	wg.AddDependency("a", "b", "import")

	if len(wg.Edges) != 1 {
		t.Errorf("expected 1 edge after dedup, got %d", len(wg.Edges))
	}
}

func TestComputeBlastRadius_Dedup(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "core", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "app", DefaultBranch: "main"})

	wg.addEdgeUnchecked("app", "core", "import")
	wg.addEdgeUnchecked("app", "core", "import")

	affected, err := wg.ComputeBlastRadius(context.Background(), "core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(affected) != 1 {
		t.Errorf("expected 1 affected after dedup, got %d", len(affected))
	}
}

func TestComputeDependencies_Transitive(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "lib-a", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "lib-b", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "lib-c", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "app", DefaultBranch: "main"})

	wg.AddDependency("app", "lib-a", "import")
	wg.AddDependency("lib-a", "lib-b", "import")
	wg.AddDependency("lib-b", "lib-c", "import")

	deps, err := wg.ComputeDependencies(context.Background(), "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected 3 upstream deps, got %d: %v", len(deps), deps)
	}
}

func TestComputeDependencies_MissingProject(t *testing.T) {
	wg := NewWorkspaceGraph()
	_, err := wg.ComputeDependencies(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for missing project")
	}
}

func TestComputeDependencies_NoUpstream(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "root", DefaultBranch: "main"})

	deps, err := wg.ComputeDependencies(context.Background(), "root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 upstream deps for root, got %d", len(deps))
	}
}

func TestHasCycles_NoCycles(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "a", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "b", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "c", DefaultBranch: "main"})

	wg.AddDependency("a", "b", "import")
	wg.AddDependency("b", "c", "import")

	if wg.HasCycles() {
		t.Fatal("expected no cycles in acyclic graph")
	}
}

func TestHasCycles_WithCycle(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "a", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "b", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "c", DefaultBranch: "main"})

	wg.AddDependency("a", "b", "import")
	wg.AddDependency("b", "c", "import")
	wg.AddDependency("c", "a", "import")

	if !wg.HasCycles() {
		t.Fatal("expected HasCycles to detect the cycle")
	}
}

func TestHasCycles_SelfCycle(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "a", DefaultBranch: "main"})
	wg.AddDependency("a", "a", "import")

	if !wg.HasCycles() {
		t.Fatal("expected HasCycles to detect self-cycle")
	}
}

func TestHasCycles_EmptyGraph(t *testing.T) {
	wg := NewWorkspaceGraph()
	if wg.HasCycles() {
		t.Fatal("expected no cycles in empty graph")
	}
}

func TestComputeBlastRadius_DeterministicOrder(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "core", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "z-app", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "a-svc", DefaultBranch: "main"})

	wg.AddDependency("z-app", "core", "import")
	wg.AddDependency("a-svc", "core", "import")

	r1, _ := wg.ComputeBlastRadius(context.Background(), "core")
	r2, _ := wg.ComputeBlastRadius(context.Background(), "core")

	if len(r1) != len(r2) {
		t.Fatal("blast radius results differ in length between runs")
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("blast radius order differs: run1=%v run2=%v", r1, r2)
		}
	}
}
