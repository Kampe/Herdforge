package harvest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/resources"
)

var execCommandContext = exec.CommandContext

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
	repoRoot      string
	DiskAdmission resources.DiskAdmission
}

func NewHarvester(repoRoot string) *Harvester {
	return &Harvester{repoRoot: repoRoot, DiskAdmission: resources.NewCapacityGate(resources.OSBackend{}, resources.DefaultDiskPolicy())}
}

func (h *Harvester) Harvest(ctx context.Context) (*HarvestResult, error) {
	return h.harvest(ctx, true)
}

// HarvestReadOnly inventories existing refs without fetching or otherwise
// fabricating a mutation. Dry-run integration uses this path.
func (h *Harvester) HarvestReadOnly(ctx context.Context) (*HarvestResult, error) {
	return h.harvest(ctx, false)
}

func (h *Harvester) harvest(ctx context.Context, fetch bool) (*HarvestResult, error) {
	result := &HarvestResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	worktrees, err := h.listWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	eligible := make([]string, 0, len(worktrees))
	for _, wt := range worktrees {
		canonical, _ := filepath.EvalSymlinks(h.repoRoot)
		wtCanonical, _ := filepath.EvalSymlinks(wt)
		if canonical != "" && wtCanonical != "" && canonical == wtCanonical {
			continue
		}
		eligible = append(eligible, wt)
	}
	// Capacity is checked before goroutine fan-out so a failed probe cannot
	// start concurrent fetches or leave a partially-mutating harvest.
	if fetch {
		requirement, err := resources.AggregateDiskRequirement(resources.DefaultMergeRequirement(), resources.DefaultWorktreeCreateRequirement())
		if err != nil {
			return nil, fmt.Errorf("disk capacity gate: invalid harvest requirement")
		}
		for _, wt := range eligible {
			if err := h.admitDisk("harvest_fetch", wt, requirement); err != nil {
				return nil, err
			}
		}
	}

	for _, wt := range eligible {

		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			u, err := h.checkUnmergedMode(ctx, path, false, fetch)
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

func (h *Harvester) admitDisk(operation, worktreePath string, requirement resources.DiskRequirement) error {
	if h == nil || h.DiskAdmission == nil {
		return fmt.Errorf("disk capacity gate unavailable for %s", operation)
	}
	repo, err := resources.ResolveExistingPath(h.repoRoot)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve repository volume: %w", err)
	}
	worktree, err := resources.ResolveExistingPath(worktreePath)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve worktree volume: %w", err)
	}
	tmp, err := resources.ResolveExistingPath(os.TempDir())
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve temporary volume: %w", err)
	}
	decision := h.DiskAdmission.Admit(resources.DiskRequest{
		Operation: operation, Path: repo, TempPath: tmp,
		RequiredBytes: requirement.Bytes, RequiredInodes: requirement.Inodes,
		AdditionalPaths: []string{worktree},
	})
	if decision.Allowed {
		return nil
	}
	evidence, _ := json.Marshal(decision.Evidence)
	return fmt.Errorf("disk capacity gate blocked: state=%s evidence=%s", decision.State, evidence)
}

func (h *Harvester) listWorktrees(ctx context.Context) ([]string, error) {
	cmd := execCommandContext(ctx, "git", "worktree", "list", "--porcelain")
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
	return h.checkUnmergedMode(ctx, worktreePath, false, true)
}

func (h *Harvester) checkUnmergedMode(ctx context.Context, worktreePath string, strict, fetch bool) (*UnmergedWork, error) {
	branchCmd := execCommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = worktreePath
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git worktree: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))

	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil, nil
	}

	if fetch {
		requirement, err := resources.AggregateDiskRequirement(resources.DefaultMergeRequirement(), resources.DefaultWorktreeCreateRequirement())
		if err != nil {
			return nil, fmt.Errorf("disk capacity gate: invalid harvest requirement")
		}
		if err := h.admitDisk("harvest_fetch", worktreePath, requirement); err != nil {
			return nil, err
		}
		fetchCmd := execCommandContext(ctx, "git", "fetch", "origin", "main")
		fetchCmd.Dir = worktreePath
		if err := fetchCmd.Run(); err != nil && strict {
			return nil, fmt.Errorf("git fetch origin main: %w", err)
		}
	}

	cherryCmd := execCommandContext(ctx, "git", "cherry", "origin/main", branch)
	cherryCmd.Dir = worktreePath
	cherryOut, err := cherryCmd.Output()
	if err != nil {
		if strict {
			return nil, fmt.Errorf("git cherry origin/main %s: %w", branch, err)
		}
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
	cmd := execCommandContext(ctx, "herdr", args...)
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
