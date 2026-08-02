package review

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

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

type ReviewEngine struct {
	RepoRoot string
}

func NewReviewEngine(repoRoot string) *ReviewEngine {
	return &ReviewEngine{RepoRoot: repoRoot}
}

// ClassifyRiskTier determines risk tier based on changed file patterns (porting bin/herd-review-classify)
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

// ComputePatchID computes git patch-id for a commit to suppress zombie backlog loops (CHA-916 / bin/herd-drain)
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
