package review

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/residual"
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

// ReviewFunc is the per-reviewer execution boundary. Production wires this
// to the harness adapter; tests inject a fake. Each call runs one reviewer
// on fresh context — the jury's whole point is that no reviewer sees
// another's work, and the author never grades its own exam.
type ReviewFunc func(ctx context.Context, reviewerModel string, packet Packet) (ReviewVerdict, error)

// JuryVerdict is the structured outcome of a multi-reviewer jury vote.
type JuryVerdict struct {
	Packet   Packet        `json:"packet"`
	Verdict  ReviewVerdict `json:"verdict"`
	Votes    []JuryVote    `json:"votes"`
	JurySize int           `json:"jury_size"`
	Passes   int           `json:"passes"`
	Fails    int           `json:"fails"`
	Stales   int           `json:"stales"`
}

// JuryVote is one reviewer's verdict.
type JuryVote struct {
	Reviewer string        `json:"reviewer"`
	Family   ModelFamily   `json:"family"`
	Verdict  ReviewVerdict `json:"verdict"`
	Err      string        `json:"err,omitempty"`
}

// JurySize is the default number of reviewers for an R3 jury.
const JurySize = 3

// SelectJury picks `size` reviewers from `available`, each from a different
// model family than the author and, where possible, from different families
// than each other. This is the "fresh context, outsider" rule: the maker
// never grades its own exam, and no two jurors share a family to avoid
// correlated bias (e.g. two Anthropic models both preferring Anthropic output).
//
// Fallback: if fewer distinct families are available than `size`, the
// remaining slots are filled with cross-family reviewers (different from
// the author) without the distinct-family constraint, so a jury can still
// form when only two families are available.
func SelectJury(authorModel string, available []string, size int) ([]string, error) {
	if size <= 0 {
		size = JurySize
	}
	reg := NewFamilyRegistry()
	authorFamily := reg.Lookup(authorModel)

	usedFamilies := map[ModelFamily]bool{authorFamily: true}
	var jury []string

	for _, rev := range available {
		if len(jury) >= size {
			break
		}
		fam := reg.Lookup(rev)
		if fam == authorFamily {
			continue
		}
		if !usedFamilies[fam] {
			jury = append(jury, rev)
			usedFamilies[fam] = true
		}
	}

	if len(jury) < size {
		for _, rev := range available {
			if len(jury) >= size {
				break
			}
			fam := reg.Lookup(rev)
			if fam == authorFamily {
				continue
			}
			alreadySelected := false
			for _, j := range jury {
				if j == rev {
					alreadySelected = true
					break
				}
			}
			if !alreadySelected {
				jury = append(jury, rev)
			}
		}
	}

	if len(jury) == 0 {
		return nil, fmt.Errorf("jury: no cross-family reviewers available for author family %s", authorModel)
	}
	return jury, nil
}

// EvaluateJury runs `size` reviewers in parallel on fresh context and
// requires majority consensus. A PASS requires >50% of non-stale votes to
// be PASS. A FAIL requires >50% to be FAIL. If no majority is reached
// (including all-stale or a tie), the verdict is FAIL — fail-closed is
// the article's "fresh context is what turns a check into an actual check."
//
// The author model is excluded from the jury by construction (SelectJury
// filters it). Each reviewer runs independently; no reviewer sees another's
// output before casting its vote.
func EvaluateJury(ctx context.Context, packet Packet, authorModel string, available []string, size int, review ReviewFunc) (*JuryVerdict, error) {
	if review == nil {
		return nil, fmt.Errorf("jury: review function is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	jury, err := SelectJury(authorModel, available, size)
	if err != nil {
		return nil, err
	}
	if len(jury) < size {
		return nil, fmt.Errorf("jury: quorum not met: requested %d reviewers, only %d cross-family available", size, len(jury))
	}

	reg := NewFamilyRegistry()
	votes := make([]JuryVote, len(jury))
	var wg sync.WaitGroup

	for i, model := range jury {
		wg.Add(1)
		go func(idx int, m string) {
			defer wg.Done()
			verdict, vErr := review(ctx, m, packet)
			v := JuryVote{
				Reviewer: m,
				Family:   reg.Lookup(m),
				Verdict:  verdict,
			}
			if vErr != nil {
				v.Err = vErr.Error()
				v.Verdict = VerdictFail
			}
			votes[idx] = v
		}(i, model)
	}
	wg.Wait()

	jv := &JuryVerdict{
		Packet:   packet,
		Votes:    votes,
		JurySize: len(jury),
	}
	for _, v := range votes {
		switch v.Verdict {
		case VerdictPass:
			jv.Passes++
		case VerdictFail:
			jv.Fails++
		case VerdictStale:
			jv.Stales++
		default:
			jv.Fails++
		}
	}
	jv.Verdict = majorityVerdict(jv.Passes, jv.Fails, jv.Stales)
	return jv, nil
}

// majorityVerdict computes the jury's consensus. Fail-closed: ties and
// all-stale produce FAIL, never PASS.
func majorityVerdict(passes, fails, stales int) ReviewVerdict {
	total := passes + fails + stales
	if total == 0 {
		return VerdictFail
	}
	threshold := total / 2
	if passes > threshold {
		return VerdictPass
	}
	return VerdictFail
}

// ShouldUseJury reports whether a risk tier requires a jury review rather
// than a single cross-family reviewer. R3 (auth, secrets, money, security-
// critical logic) requires a jury; R0-R2 use the existing single-reviewer
// or mechanical-merge paths.
func ShouldUseJury(tier RiskTier) bool {
	return tier == TierR3RiskCritical
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
	// Residuals are reviewed as context; they cannot turn a required criterion
	// into a PASS. Merge admission verifies their linkage independently.
	Residuals []residual.Record `json:"residuals,omitempty"`
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
