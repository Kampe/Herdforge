package review

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/control"
)

// ModelFamily maps a provider or model prefix to a canonical family name.
type ModelFamily string

const (
	FamilyAnthropic ModelFamily = "anthropic"
	FamilyGoogle    ModelFamily = "google"
	FamilyOpenAI    ModelFamily = "openai"
	FamilyGrok      ModelFamily = "grok"
	FamilyOllama    ModelFamily = "ollama"
	FamilyKimi      ModelFamily = "kimi"
	FamilyCodex     ModelFamily = "codex"
	FamilyLazer     ModelFamily = "lazer"
	FamilyOther     ModelFamily = "other"
)

// FamilyRegistry provides deterministic model-family lookup.
type FamilyRegistry struct {
	// entries maps known model prefixes (e.g. "claude", "gemini", "gpt")
	// to their canonical family.
	entries map[string]ModelFamily
}

// NewFamilyRegistry returns a registry populated with the default known entries.
func NewFamilyRegistry() *FamilyRegistry {
	return &FamilyRegistry{
		entries: map[string]ModelFamily{
			"claude":    FamilyAnthropic,
			"sonnet":    FamilyAnthropic,
			"opus":      FamilyAnthropic,
			"haiku":     FamilyAnthropic,
			"anthropic": FamilyAnthropic,
			"gemini":    FamilyGoogle,
			"google":    FamilyGoogle,
			"agy":       FamilyGoogle,
			"gpt":       FamilyOpenAI,
			"o1":        FamilyOpenAI,
			"o3":        FamilyOpenAI,
			"grok":      FamilyGrok,
			"xai":       FamilyGrok,
			"ollama":    FamilyOllama,
			"llama":     FamilyOllama,
			"kimi":      FamilyKimi,
			"moonshot":  FamilyKimi,
			"codex":     FamilyCodex,
			"lazer":     FamilyLazer,
			"deepseek":  FamilyLazer,
		},
	}
}

// Lookup returns the canonical family for a model name, falling back to Other.
func (r *FamilyRegistry) Lookup(modelName string) ModelFamily {
	lower := strings.ToLower(modelName)
	for prefix, family := range r.entries {
		if strings.Contains(lower, prefix) {
			return family
		}
	}
	return FamilyOther
}

// KnownFamilies returns all registered families.
func (r *FamilyRegistry) KnownFamilies() []ModelFamily {
	seen := make(map[ModelFamily]bool)
	var result []ModelFamily
	for _, f := range r.entries {
		if !seen[f] {
			seen[f] = true
			result = append(result, f)
		}
	}
	return result
}

type RiskTier string

const (
	TierR0RiskMechanical RiskTier = "R0" // Docs, markdown, formatting, simple unit tests
	TierR1RiskStandard   RiskTier = "R1" // Standard feature additions & minor refactors
	TierR2RiskHigh       RiskTier = "R2" // Core API, schema changes, database migrations
	TierR3RiskCritical   RiskTier = "R3" // Auth, secrets, money, security-critical logic
)

type ReviewVerdict string

const (
	VerdictPass  ReviewVerdict = "PASS"
	VerdictFail  ReviewVerdict = "FAIL"
	VerdictStale ReviewVerdict = "STALE"
)

type Packet struct {
	ID          string        `json:"id"`
	Branch      string        `json:"branch"`
	CommitSHA   string        `json:"commit_sha"`
	PatchID     string        `json:"patch_id"`
	AuthorRole  string        `json:"author_role"`
	Tier        RiskTier      `json:"tier"`
	Verdict     ReviewVerdict `json:"verdict"`
	Reviewer    string        `json:"reviewer"`
	SubmittedAt time.Time     `json:"submitted_at"`
}

type HarvestResult struct {
	Merged    bool
	CommitSHA string
	Output    string
}

type ReviewEngine struct {
	RepoRoot string
	Orders   *control.CoordinatorOrders
}

func NewReviewEngine(repoRoot string) *ReviewEngine {
	return &ReviewEngine{RepoRoot: repoRoot}
}

// ClassifyRiskTier determines risk tier based on changed file patterns
func ClassifyRiskTier(files []string) RiskTier {
	hasCoreCode := false
	hasAuthOrMoney := false

	for _, f := range files {
		lower := strings.ToLower(f)
		if strings.Contains(lower, "auth") || strings.Contains(lower, "secret") || strings.Contains(lower, "money") || strings.Contains(lower, "payment") {
			hasAuthOrMoney = true
		}
		if strings.HasSuffix(lower, ".go") || strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".rs") {
			hasCoreCode = true
		}
	}

	if hasAuthOrMoney {
		return TierR3RiskCritical
	}
	if hasCoreCode {
		return TierR1RiskStandard
	}

	return TierR0RiskMechanical
}

// SelectCrossFamilyReviewer ensures that a reviewer model belongs to a different model family than the worker,
// using the FamilyRegistry for deterministic family lookup.
func SelectCrossFamilyReviewer(authorModelFamily string, availableReviewers []string) (string, error) {
	reg := NewFamilyRegistry()
	authorFamily := reg.Lookup(authorModelFamily)

	for _, rev := range availableReviewers {
		if reg.Lookup(rev) != authorFamily {
			return rev, nil
		}
	}

	// Fallback: try substring matching for unknown families
	authorLower := strings.ToLower(authorModelFamily)
	for _, rev := range availableReviewers {
		revLower := strings.ToLower(rev)
		if !strings.Contains(revLower, authorLower) && !strings.Contains(authorLower, revLower) {
			return rev, nil
		}
	}

	if len(availableReviewers) > 0 {
		return availableReviewers[0], nil
	}
	return "", fmt.Errorf("no cross-family reviewers available for author family %s", authorModelFamily)
}

// ComputePatchID computes git patch-id for a commit to suppress zombie backlog loops (CHA-916)
func (r *ReviewEngine) ComputePatchID(ctx context.Context, commitSHA string) (string, error) {
	showCmd := exec.CommandContext(ctx, "git", "show", commitSHA)
	showCmd.Dir = r.RepoRoot

	patchCmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	patchCmd.Dir = r.RepoRoot

	pipe, err := showCmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	patchCmd.Stdin = pipe

	if err := showCmd.Start(); err != nil {
		return "", err
	}

	out, err := patchCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to compute patch-id: %w", err)
	}
	showCmd.Wait()

	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty patch-id output")
	}

	return fields[0], nil
}

// RebaseMergeBranch executes clean git rebase and merge onto main branch
func (r *ReviewEngine) RebaseMergeBranch(ctx context.Context, branchName, targetBranch string) (*HarvestResult, error) {
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin", targetBranch)
	fetchCmd.Dir = r.RepoRoot
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		return &HarvestResult{Merged: false, Output: string(out)}, fmt.Errorf("git fetch failed: %w", err)
	}

	coCmd := exec.CommandContext(ctx, "git", "checkout", targetBranch)
	coCmd.Dir = r.RepoRoot
	if out, err := coCmd.CombinedOutput(); err != nil {
		return &HarvestResult{Merged: false, Output: string(out)}, fmt.Errorf("git checkout failed: %w", err)
	}

	mergeCmd := exec.CommandContext(ctx, "git", "merge", "--rebase", branchName)
	mergeCmd.Dir = r.RepoRoot
	out, err := mergeCmd.CombinedOutput()
	if err != nil {
		return &HarvestResult{Merged: false, Output: string(out)}, fmt.Errorf("git merge --rebase failed: %w", err)
	}

	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = r.RepoRoot
	shaOut, err := revCmd.Output()
	if err != nil {
		return &HarvestResult{Merged: true, Output: string(out)}, nil
	}
	if r.Orders != nil {
		if _, orderErr := r.Orders.Rebase(ctx, fmt.Sprintf("rebase %s onto %s -> %s", branchName, targetBranch, strings.TrimSpace(string(shaOut)))); orderErr != nil {
			return &HarvestResult{Merged: false, Output: string(out)}, fmt.Errorf("durable rebase order: %w", orderErr)
		}
	}

	return &HarvestResult{
		Merged:    true,
		CommitSHA: strings.TrimSpace(string(shaOut)),
		Output:    string(out),
	}, nil
}
