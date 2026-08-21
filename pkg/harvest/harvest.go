package harvest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/procsignal"
	"github.com/Kampe/Herdforge/pkg/resources"
)

var execCommandContext = procsignal.CommandContext

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

	// FAC-543: worktrees of one repository SHARE an object store and refs, so
	// `git fetch origin main` from any of them updates origin/main for all.
	// Fetching per worktree therefore did N identical network round-trips —
	// 93 worktrees x ~0.9s made `unmerged --all` take ~41s and look hung to
	// any caller with a sane timeout. Dedupe by git common dir so the fetch
	// happens once per repository per Harvester.
	fetchOnce sync.Map // common git dir -> *sync.Once
	fetchErr  sync.Map // common git dir -> error
}

// fetchOriginMainOnce runs `git fetch origin main` at most once per repository
// (keyed by git common dir) for this Harvester, returning the same result to
// every caller that shares the store.
func (h *Harvester) fetchOriginMainOnce(ctx context.Context, dir string) error {
	key := dir
	if out, err := func() ([]byte, error) {
		c := execCommandContext(ctx, "git", "rev-parse", "--git-common-dir")
		c.Dir = dir
		return c.Output()
	}(); err == nil {
		if common := strings.TrimSpace(string(out)); common != "" {
			if abs, absErr := filepath.Abs(filepath.Join(dir, common)); absErr == nil {
				key = abs
			} else {
				key = common
			}
		}
	}
	onceAny, _ := h.fetchOnce.LoadOrStore(key, &sync.Once{})
	once := onceAny.(*sync.Once)
	once.Do(func() {
		cmd := execCommandContext(ctx, "git", "fetch", "origin", "main")
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			h.fetchErr.Store(key, err)
		}
	})
	if e, ok := h.fetchErr.Load(key); ok {
		if err, isErr := e.(error); isErr {
			return err
		}
	}
	return nil
}

// preadmittedFetch is an internal capability minted only after the complete
// harvest batch has been planned and admitted. It cannot be supplied by
// callers of the public direct-fetch APIs.
type preadmittedFetch struct {
	batch         *preadmittedBatch
	originalPath  string
	canonicalPath string
	planDigest    string
	scope         string
	worktreePath  string
}

type preadmittedBatch struct {
	digest string
	tokens map[string]*preadmittedFetch
}

type fetchMode uint8

const (
	noFetch fetchMode = iota
	directFetch
	batchFetch
)

type harvestFetchItem struct {
	originalPath  string
	canonicalPath string
	request       resources.DiskRequest
	planDigest    string
	token         *preadmittedFetch
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
	var fatalErr error

	worktrees, err := h.listWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	eligible := worktrees
	// Capacity is checked before goroutine fan-out so a failed probe cannot
	// start concurrent fetches or leave a partially-mutating harvest.
	mode := noFetch
	items := make([]*harvestFetchItem, 0, len(eligible))
	if fetch {
		mode = batchFetch
	}
	if mode == batchFetch && len(eligible) > 0 {
		requirement, err := resources.AggregateDiskRequirement(resources.DefaultMergeRequirement(), resources.DefaultWorktreeCreateRequirement())
		if err != nil {
			return nil, fmt.Errorf("disk capacity gate: invalid harvest requirement: %w", err)
		}
		items, err = h.prepareHarvestFetchItems(ctx, eligible, requirement, true)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return result, nil
		}
		requests := make([]resources.DiskRequest, 0, len(items))
		for _, item := range items {
			requests = append(requests, item.request)
		}
		batch, err := h.admitHarvestBatch(requests)
		if err != nil {
			return nil, err
		}
		if err := bindHarvestTokens(items, batch, requirement); err != nil {
			return nil, err
		}
		if err := validateHarvestTokensAfterAdmission(items, batch, requirement); err != nil {
			return nil, err
		}
		// FAC-543: bound the fan-out. Previously every eligible worktree was
		// launched at once — measured at 87 concurrent `git` processes on a
		// 93-worktree repo, which saturated the machine (514% CPU, 146s SYSTEM
		// time from contention) and made `unmerged --all` take ~42s. Work is
		// per-worktree independent, so a bounded pool does the same work
		// without thrashing. Same defect family as FAC-481's unbounded drain.
		limit := runtime.NumCPU()
		if limit < 2 {
			limit = 2
		}
		if limit > 8 {
			limit = 8
		}
		sem := make(chan struct{}, limit)
		for _, item := range items {
			token := item.token
			wg.Add(1)
			sem <- struct{}{}
			go func(item *harvestFetchItem, admission *preadmittedFetch) {
				defer func() { <-sem }()
				defer wg.Done()
				u, err := h.checkUnmergedMode(ctx, item.canonicalPath, false, batchFetch, admission)
				if err != nil {
					var diskErr *resources.DiskAdmissionError
					mu.Lock()
					if errors.As(err, &diskErr) {
						if fatalErr == nil {
							fatalErr = err
						}
					} else {
						result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", item.originalPath, err))
					}
					mu.Unlock()
					return
				}
				if u != nil {
					mu.Lock()
					result.UnmergedWorktrees = append(result.UnmergedWorktrees, *u)
					mu.Unlock()
				}
			}(item, token)
		}
		wg.Wait()
		if fatalErr != nil {
			return nil, fatalErr
		}
		return result, nil
	}

	for _, wt := range eligible {
		wg.Add(1)
		go func(path string, admission *preadmittedFetch, fetchMode fetchMode) {
			defer wg.Done()
			u, err := h.checkUnmergedMode(ctx, path, false, fetchMode, admission)
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
		}(wt, nil, noFetch)
	}
	wg.Wait()

	return result, nil
}

func (h *Harvester) prepareHarvestFetchItems(ctx context.Context, worktrees []string, requirement resources.DiskRequirement, skipRepo bool) ([]*harvestFetchItem, error) {
	if h == nil || h.DiskAdmission == nil {
		return nil, harvestDiskError(resources.DiskReasonUnavailable, requirement)
	}
	repo, err := resources.ResolveExistingPath(h.repoRoot)
	if err != nil {
		return nil, harvestDiskError(resources.DiskReasonUnavailable, requirement)
	}
	tmp, err := resources.ResolveExistingPath(os.TempDir())
	if err != nil {
		return nil, harvestDiskError(resources.DiskReasonUnavailable, requirement)
	}
	items := make([]*harvestFetchItem, 0, len(worktrees))
	seenOriginal := make(map[string]struct{}, len(worktrees))
	seenCanonical := make(map[string]struct{}, len(worktrees))
	for _, worktreePath := range worktrees {
		worktree, err := resources.ResolveExistingPath(worktreePath)
		if err != nil {
			return nil, harvestDiskError(resources.DiskReasonUnavailable, requirement)
		}
		if _, duplicate := seenOriginal[worktreePath]; duplicate {
			return nil, harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		if skipRepo && worktree == repo {
			nonMutating, err := h.isNonMutatingRoot(ctx, repo)
			if err != nil {
				return nil, harvestDiskError(resources.DiskReasonUnavailable, requirement)
			}
			if nonMutating {
				seenOriginal[worktreePath] = struct{}{}
				continue
			}
		}
		if _, duplicate := seenCanonical[worktree]; duplicate {
			return nil, harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		seenOriginal[worktreePath] = struct{}{}
		seenCanonical[worktree] = struct{}{}
		items = append(items, &harvestFetchItem{originalPath: worktreePath, canonicalPath: worktree, request: resources.DiskRequest{
			Operation: "harvest_fetch", Path: repo, TempPath: tmp,
			RequiredBytes: requirement.Bytes, RequiredInodes: requirement.Inodes,
			AdditionalPaths: []string{worktree},
		}})
	}
	return items, nil
}

func (h *Harvester) isNonMutatingRoot(ctx context.Context, root string) (bool, error) {
	cmd := execCommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "main", "master", "HEAD":
		return true, nil
	default:
		return false, nil
	}
}

func (h *Harvester) admitHarvestBatch(requests []resources.DiskRequest) (*preadmittedBatch, error) {
	provider, ok := h.DiskAdmission.(resources.DiskPlanProvider)
	if !ok {
		return nil, harvestDiskError(resources.DiskReasonUnavailable, resources.DiskRequirement{})
	}
	if _, ok := h.DiskAdmission.(resources.BatchDiskAdmission); !ok {
		return nil, harvestDiskError(resources.DiskReasonUnavailable, resources.DiskRequirement{})
	}
	plan, err := provider.PlanDiskAdmission(requests)
	if err != nil {
		return nil, fmt.Errorf("disk capacity gate: plan harvest batch: %w", err)
	}
	if err := resources.AdmitDiskPlan(h.DiskAdmission, plan); err != nil {
		return nil, fmt.Errorf("disk capacity gate: admit harvest batch: %w", err)
	}
	digestBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, harvestDiskError(resources.DiskReasonInvalid, resources.DiskRequirement{})
	}
	digest := sha256.Sum256(digestBytes)
	batch := &preadmittedBatch{digest: hex.EncodeToString(digest[:]), tokens: make(map[string]*preadmittedFetch, len(requests))}
	return batch, nil
}

func bindHarvestTokens(items []*harvestFetchItem, batch *preadmittedBatch, requirement resources.DiskRequirement) error {
	if batch == nil || batch.digest == "" {
		return harvestDiskError(resources.DiskReasonInvalid, requirement)
	}
	byOriginal := make(map[string]*harvestFetchItem, len(items))
	byCanonical := make(map[string]*harvestFetchItem, len(items))
	for _, item := range items {
		if item == nil || item.originalPath == "" || item.canonicalPath == "" || item.token != nil {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		if _, duplicate := byOriginal[item.originalPath]; duplicate {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		if _, duplicate := byCanonical[item.canonicalPath]; duplicate {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		item.planDigest = batch.digest
		item.token = &preadmittedFetch{batch: batch, originalPath: item.originalPath, canonicalPath: item.canonicalPath, planDigest: item.planDigest, scope: resources.CapacityScopeForPaths(item.canonicalPath), worktreePath: item.originalPath}
		if batch.tokens[item.canonicalPath] != nil {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		batch.tokens[item.canonicalPath] = item.token
		byOriginal[item.originalPath] = item
		byCanonical[item.canonicalPath] = item
	}
	if len(byOriginal) != len(items) || len(byCanonical) != len(items) || len(batch.tokens) != len(items) {
		return harvestDiskError(resources.DiskReasonInvalid, requirement)
	}
	for _, item := range items {
		token := item.token
		if token == nil || item.planDigest == "" || token.batch != batch || token.batch.digest != item.planDigest || token.planDigest != item.planDigest || token.originalPath != item.originalPath || token.canonicalPath != item.canonicalPath || token.scope != resources.CapacityScopeForPaths(item.canonicalPath) || token.batch.tokens[item.canonicalPath] != token {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
	}
	return nil
}

func validateHarvestTokensAfterAdmission(items []*harvestFetchItem, batch *preadmittedBatch, requirement resources.DiskRequirement) error {
	if batch == nil || batch.digest == "" {
		return harvestDiskError(resources.DiskReasonInvalid, requirement)
	}
	byOriginal := make(map[string]*harvestFetchItem, len(items))
	byCanonical := make(map[string]*harvestFetchItem, len(items))
	for _, item := range items {
		if item == nil || item.token == nil || item.originalPath == "" || item.canonicalPath == "" {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		canonical, err := resources.ResolveExistingPath(item.originalPath)
		if err != nil || canonical != item.canonicalPath {
			return harvestDiskError(resources.DiskReasonUnavailable, requirement)
		}
		if _, duplicate := byOriginal[item.originalPath]; duplicate {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		if _, duplicate := byCanonical[item.canonicalPath]; duplicate {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		token := item.token
		if token.batch != batch || token.planDigest != batch.digest || token.originalPath != item.originalPath || token.canonicalPath != item.canonicalPath || token.scope != resources.CapacityScopeForPaths(item.canonicalPath) || batch.tokens[item.canonicalPath] != token {
			return harvestDiskError(resources.DiskReasonInvalid, requirement)
		}
		byOriginal[item.originalPath] = item
		byCanonical[item.canonicalPath] = item
	}
	if len(byOriginal) != len(items) || len(byCanonical) != len(items) || len(batch.tokens) != len(items) {
		return harvestDiskError(resources.DiskReasonInvalid, requirement)
	}
	return nil
}

func harvestDiskError(reason string, requirement resources.DiskRequirement) error {
	return &resources.DiskAdmissionError{Scope: "harvest", Decision: resources.DiskDecision{
		State: resources.DiskBlocked,
		Evidence: resources.DiskEvidence{
			Kind: "disk_pressure", Reason: reason, Operation: "harvest_batch",
			RequiredBytes: requirement.Bytes, RequiredInodes: requirement.Inodes,
			NextAction: resources.DiskActionRetryProbe,
		},
	}}
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
	return h.checkUnmergedMode(ctx, worktreePath, false, directFetch, nil)
}

func (h *Harvester) checkUnmergedMode(ctx context.Context, worktreePath string, strict bool, mode fetchMode, admission *preadmittedFetch) (*UnmergedWork, error) {
	effectivePath := worktreePath
	if mode == directFetch {
		var err error
		admission, err = h.admitDirectFetch(ctx, worktreePath)
		if err != nil {
			return nil, err
		}
		canonical, err := resources.ResolveExistingPath(worktreePath)
		if err != nil || admission == nil || canonical != admission.canonicalPath || admission.originalPath != worktreePath || admission.batch == nil || admission.batch.digest == "" || admission.planDigest != admission.batch.digest || admission.scope != resources.CapacityScopeForPaths(canonical) || admission.batch.tokens[canonical] != admission {
			return nil, harvestDiskError(resources.DiskReasonUnavailable, resources.DiskRequirement{})
		}
		effectivePath = admission.canonicalPath
	} else if mode == batchFetch {
		if admission == nil || admission.batch == nil || admission.batch.digest == "" || admission.planDigest == "" || admission.planDigest != admission.batch.digest || admission.originalPath == "" || admission.canonicalPath != worktreePath || admission.scope != resources.CapacityScopeForPaths(worktreePath) || admission.batch.tokens[worktreePath] != admission {
			return nil, harvestDiskError(resources.DiskReasonInvalid, resources.DiskRequirement{})
		}
		effectivePath = admission.canonicalPath
	}

	branchCmd := execCommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = effectivePath
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git worktree: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil, nil
	}

	if mode == directFetch || mode == batchFetch {
		if err := h.fetchOriginMainOnce(ctx, effectivePath); err != nil && strict {
			return nil, fmt.Errorf("git fetch origin main: %w", err)
		}
	}

	cherryCmd := execCommandContext(ctx, "git", "cherry", "origin/main", branch)
	cherryCmd.Dir = effectivePath
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
		WorktreePath: effectivePath,
		Branch:       branch,
		Unmerged:     unique,
	}, nil
}

func (h *Harvester) admitDirectFetch(ctx context.Context, worktreePath string) (*preadmittedFetch, error) {
	requirement, err := resources.AggregateDiskRequirement(resources.DefaultMergeRequirement(), resources.DefaultWorktreeCreateRequirement())
	if err != nil {
		return nil, fmt.Errorf("disk capacity gate: invalid direct-fetch requirement: %w", err)
	}
	items, err := h.prepareHarvestFetchItems(ctx, []string{worktreePath}, requirement, false)
	if err != nil {
		return nil, err
	}
	if len(items) != 1 {
		return nil, harvestDiskError(resources.DiskReasonInvalid, requirement)
	}
	requests := []resources.DiskRequest{items[0].request}
	batch, err := h.admitHarvestBatch(requests)
	if err != nil {
		return nil, err
	}
	if err := bindHarvestTokens(items, batch, requirement); err != nil {
		return nil, err
	}
	return items[0].token, nil
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
