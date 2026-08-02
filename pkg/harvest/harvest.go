package harvest

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type UnmergedWork struct {
	WorktreePath string   `json:"worktree_path"`
	Branch       string   `json:"branch"`
	Unmerged     []string `json:"unmerged_commits"`
}

type HarvestResult struct {
	UnmergedWorktrees []UnmergedWork `json:"unmerged_worktrees"`
	Errors            []string       `json:"errors,omitempty"`
}

type Harvester struct {
	repoRoot string
}

func NewHarvester(repoRoot string) *Harvester {
	return &Harvester{repoRoot: repoRoot}
}

func (h *Harvester) Harvest(ctx context.Context) (*HarvestResult, error) {
	result := &HarvestResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	worktrees, err := h.listWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	for _, wt := range worktrees {
		canonical, _ := filepath.EvalSymlinks(h.repoRoot)
		wtCanonical, _ := filepath.EvalSymlinks(wt)
		if canonical != "" && wtCanonical != "" && canonical == wtCanonical {
			continue
		}

		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			u, err := h.checkUnmerged(ctx, path)
			if err != nil {
				mu.Lock()
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
				mu.Unlock()
				return
			}
			if u != nil {
				mu.Lock()
				result.UnmergedWorktrees = append(result.UnmergedWorktrees, *u)
				mu.Unlock()
			}
		}(wt)
	}
	wg.Wait()

	return result, nil
}

func (h *Harvester) listWorktrees(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = h.repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	var paths []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "worktree ") {
			path := strings.TrimPrefix(line, "worktree ")
			paths = append(paths, path)
		}
	}
	return paths, scanner.Err()
}

func (h *Harvester) checkUnmerged(ctx context.Context, worktreePath string) (*UnmergedWork, error) {
	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = worktreePath
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git worktree: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))

	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil, nil
	}

	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin", "main")
	fetchCmd.Dir = worktreePath
	_ = fetchCmd.Run()

	cherryCmd := exec.CommandContext(ctx, "git", "cherry", "origin/main", branch)
	cherryCmd.Dir = worktreePath
	cherryOut, err := cherryCmd.Output()
	if err != nil {
		return nil, nil
	}

	var unique []string
	scanner := bufio.NewScanner(strings.NewReader(string(cherryOut)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "+ ") {
			unique = append(unique, strings.TrimPrefix(line, "+ "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(unique) == 0 {
		return nil, nil
	}

	return &UnmergedWork{
		WorktreePath: worktreePath,
		Branch:       branch,
		Unmerged:     unique,
	}, nil
}

func (h *Harvester) UnmergedWorktreeCount(ctx context.Context) (int, error) {
	result, err := h.Harvest(ctx)
	if err != nil {
		return 0, err
	}
	return len(result.UnmergedWorktrees), nil
}

// PaneAttention shells out to herdr for agent attention summary.
// Mirrors bin/herd-attention which uses herdr agent list + jq filtering.
type PaneAttention struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Role     string `json:"role"`
	Standing bool   `json:"standing"`
}

func PaneAttentionFromHerdr(ctx context.Context, workspace string) ([]PaneAttention, error) {
	args := []string{"agent", "list"}
	if workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	cmd := exec.CommandContext(ctx, "herdr", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	_ = out
	// For now return empty — herdr JSON parsing requires the actual herdr output format
	return nil, nil
}

func (h *Harvester) Summary(ctx context.Context) string {
	result, err := h.Harvest(ctx)
	if err != nil {
		return fmt.Sprintf("herd-harvest: error — %v", err)
	}
	if len(result.UnmergedWorktrees) == 0 {
		return "herd-harvest: no unmerged commits in any worktree"
	}
	return fmt.Sprintf("herd-harvest: %d worktree(s) with unmerged commits", len(result.UnmergedWorktrees))
}

func (h *Harvester) QuietSummary(ctx context.Context) string {
	c, err := h.UnmergedWorktreeCount(ctx)
	if err != nil {
		return fmt.Sprintf("herd-harvest: error — %v", err)
	}
	return fmt.Sprintf("herd-harvest: %d worktree(s) with unmerged commits", c)
}

func init() {
	_ = os.Getenv("HERD_WORKTREE")
}
