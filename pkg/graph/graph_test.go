package graph

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestWorkspaceGraph_ComputeBlastRadius(t *testing.T) {
	wg := NewWorkspaceGraph()
	wg.AddProject(&config.ProjectConfig{Name: "core-lib", DefaultBranch: "main"})
	wg.AddProject(&config.ProjectConfig{Name: "app-api", DefaultBranch: "main"})

	wg.AddDependency("app-api", "core-lib", "import")

	affected, err := wg.ComputeBlastRadius(context.Background(), "core-lib")
	if err != nil || len(affected) != 1 {
		t.Fatalf("expected 1 affected project in blast radius, got %d (err: %v)", len(affected), err)
	}

	if affected[0] != "app-api" {
		t.Errorf("expected 'app-api' in blast radius, got '%s'", affected[0])
	}
}
