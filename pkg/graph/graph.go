package graph

import (
	"context"
	"fmt"

	"sort"

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
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Type == depType {
			return
		}
	}
	g.Edges = append(g.Edges, DependencyEdge{
		From: from,
		To:   to,
		Type: depType,
	})
}

func (g *WorkspaceGraph) addEdgeUnchecked(from, to, depType string) {
	g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Type: depType})
}

// ComputeBlastRadius calculates all downstream projects transitively affected by a change in target
func (g *WorkspaceGraph) ComputeBlastRadius(ctx context.Context, targetProject string) ([]string, error) {
	if _, exists := g.Nodes[targetProject]; !exists {
		return nil, fmt.Errorf("project %s not found in workspace graph", targetProject)
	}

	depMap := make(map[string][]string)
	for _, edge := range g.Edges {
		depMap[edge.To] = append(depMap[edge.To], edge.From)
	}

	visited := map[string]bool{targetProject: true}
	var affected []string
	queue := []string{targetProject}

	for len(queue) > 0 {
		proj := queue[0]
		queue = queue[1:]

		for _, upstream := range depMap[proj] {
			if !visited[upstream] {
				visited[upstream] = true
				affected = append(affected, upstream)
				queue = append(queue, upstream)
			}
		}
	}

	sort.Strings(affected)
	return affected, nil
}

// ComputeDependencies returns all projects that targetProject depends on (upstream), transitively
func (g *WorkspaceGraph) ComputeDependencies(ctx context.Context, targetProject string) ([]string, error) {
	if _, exists := g.Nodes[targetProject]; !exists {
		return nil, fmt.Errorf("project %s not found in workspace graph", targetProject)
	}

	depMap := make(map[string][]string)
	for _, edge := range g.Edges {
		depMap[edge.From] = append(depMap[edge.From], edge.To)
	}

	visited := map[string]bool{targetProject: true}
	var upstream []string
	queue := []string{targetProject}

	for len(queue) > 0 {
		proj := queue[0]
		queue = queue[1:]

		for _, dep := range depMap[proj] {
			if !visited[dep] {
				visited[dep] = true
				upstream = append(upstream, dep)
				queue = append(queue, dep)
			}
		}
	}

	sort.Strings(upstream)
	return upstream, nil
}

// HasCycles returns true if the graph contains any cycles
func (g *WorkspaceGraph) HasCycles() bool {
	inDegree := make(map[string]int)
	for _, node := range g.Nodes {
		inDegree[node.Name] = 0
	}
	for _, edge := range g.Edges {
		inDegree[edge.To]++
	}

	queue := []string{}
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}

	visited := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		visited++

		for _, edge := range g.Edges {
			if edge.From == node {
				inDegree[edge.To]--
				if inDegree[edge.To] == 0 {
					queue = append(queue, edge.To)
				}
			}
		}
	}

	return visited != len(g.Nodes)
}
