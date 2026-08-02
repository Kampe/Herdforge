package graph

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestComputeBlastRadius_MissingProject(t *testing.T) {
	wg := NewWorkspaceGraph()
	_, err := wg.ComputeBlastRadius(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing project")
	}
}

func TestComputeBlastRadius_MultipleDownstream(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "core", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "svc-a", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "svc-b", DefaultBranch: "main"})

	wg.AddDependency("svc-a", "core", "import")
	wg.AddDependency("svc-b", "core", "import")

	affected, err := wg.ComputeBlastRadius(context.Background(), "core")
	if err != nil {
		t.Fatalf("expected clean blast radius, got err: %v", err)
	}
	if len(affected) != 2 {
		t.Errorf("expected 2 affected projects, got %d: %v", len(affected), affected)
	}
}

func TestAddProject_Nil(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(nil) // should not panic
	if len(wg.Nodes) != 0 {
		t.Errorf("expected 0 nodes after nil add, got %d", len(wg.Nodes))
	}
}

func TestAddProject_EmptyName(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: ""})
	if len(wg.Nodes) != 0 {
		t.Errorf("expected 0 nodes after empty name add, got %d", len(wg.Nodes))
	}
}
