package graph

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/config"
)

type DependencyEdge struct {
	From string
	To   string
	Type string
}

type WorkspaceGraph struct {
	Nodes map[string]*config.ProjectConfig
	Edges []DependencyEdge
}

func NewWorkspaceGraph() *WorkspaceGraph {
	return &WorkspaceGraph{
		Nodes: make(map[string]*config.ProjectConfig),
		Edges: []DependencyEdge{},
	}
}

func (g *WorkspaceGraph) AddProject(p *config.ProjectConfig) {
	if p != nil && p.Name != "" {
		g.Nodes[p.Name] = p
	}
}

func (g *WorkspaceGraph) AddDependency(from, to, depType string) {
	g.Edges = append(g.Edges, DependencyEdge{
		From: from,
		To:   to,
		Type: depType,
	})
}

// ComputeBlastRadius calculates all downstream projects affected by a change in target project
func (g *WorkspaceGraph) ComputeBlastRadius(ctx context.Context, targetProject string) ([]string, error) {
	if _, exists := g.Nodes[targetProject]; !exists {
		return nil, fmt.Errorf("project %s not found in workspace graph", targetProject)
	}

	var affected []string
	for _, edge := range g.Edges {
		if edge.To == targetProject {
			affected = append(affected, edge.From)
		}
	}

	return affected, nil
}
