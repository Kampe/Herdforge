package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/readyindex"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

const readinessBeatStateSchema = 1

type livePullRequest struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	HeadRefOID string `json:"headRefOid"`
}

type readinessBeatState struct {
	Schema     int            `json:"schema_version"`
	Conditions map[string]int `json:"conditions"`
}

// appendReadyCandidateAttention is the shipped join between the exact-ready
// projection, canonical per-SHA readiness, and live pull-request identity.
// Pull-request lookup is injected so tests never make provider calls.
func appendReadyCandidateAttention(result *attention.Result, ledgerPath string, lookup func(string) ([]livePullRequest, error)) error {
	if result == nil {
		return fmt.Errorf("herd-attention: result is required")
	}
	if lookup == nil {
		return fmt.Errorf("herd-attention: pull-request lookup is required")
	}
	candidates, err := readyAttentionCandidates(ledgerPath)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return clearReadinessBeatState(ledgerPath)
	}

	ledger, err := reviewledger.NewReadOnlyReviewLedger(".", ledgerPath)
	if err != nil {
		return fmt.Errorf("herd-attention: open review ledger: %w", err)
	}
	previous, err := loadReadinessBeatState(ledgerPath)
	if err != nil {
		return err
	}
	next := readinessBeatState{Schema: readinessBeatStateSchema, Conditions: map[string]int{}}
	findings := make([]attention.Item, 0, len(candidates))

	for _, candidate := range candidates {
		sha, err := exactCandidateSHA(candidate.SHA)
		if err != nil {
			return fmt.Errorf("herd-attention: ready index: %w", err)
		}
		readiness, err := ledger.MergeReadinessFor(sha)
		if err != nil {
			return fmt.Errorf("herd-attention: canonical readiness %s: %w", sha, err)
		}
		if !readiness.Ready {
			continue
		}

		prs := []livePullRequest(nil)
		branch := strings.TrimSpace(candidate.Branch)
		if branch != "" {
			prs, err = lookup(branch)
			if err != nil {
				return fmt.Errorf("herd-attention: live PR identity for %s (%s): %w", sha, branch, err)
			}
		}
		finding, ok := readyCandidateFinding(sha, branch, prs)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s:%s:%d", finding.Status, sha, finding.PullRequest)
		finding.Beats = previous.Conditions[key] + 1
		next.Conditions[key] = finding.Beats
		if finding.Beats > 1 {
			finding.Escalated = true
			finding.Reason = fmt.Sprintf("ESCALATED after %d beats: %s", finding.Beats, finding.Reason)
		}
		findings = append(findings, finding)
	}
	if err := saveReadinessBeatState(ledgerPath, next); err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].SHA < findings[j].SHA })
	result.Items = append(findings, result.Items...)
	if result.Counts == nil {
		result.Counts = map[attention.AttentionLevel]int{}
	}
	result.Counts[attention.LevelCritical] += len(findings)
	result.ReadyCandidates += len(findings)
	return nil
}

func readyAttentionCandidates(ledgerPath string) ([]readyindex.Entry, error) {
	entries, err := readyindex.List(readyindex.PathFor(ledgerPath))
	if err == nil {
		return entries, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("herd-attention: exact-ready index: %w", err)
	}
	ledger, err := reviewledger.NewReadOnlyReviewLedger(".", ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("herd-attention: open review ledger: %w", err)
	}
	queued, err := ledger.Queued()
	if err != nil {
		return nil, fmt.Errorf("herd-attention: rebuild exact-ready candidates: %w", err)
	}
	entries = make([]readyindex.Entry, 0, len(queued))
	for _, row := range queued {
		entries = append(entries, readyindex.Entry{SHA: row.SHA, Branch: row.Branch, Lane: row.Lane, Reviewer: row.Reviewer, Updated: row.Timestamp})
	}
	return entries, nil
}

func exactCandidateSHA(raw string) (string, error) {
	sha := strings.ToLower(strings.TrimSpace(raw))
	if len(sha) != 40 {
		return "", fmt.Errorf("candidate SHA %q is not an exact 40-character identity", raw)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", fmt.Errorf("candidate SHA %q is not hexadecimal", raw)
	}
	return sha, nil
}

func readyCandidateFinding(sha, branch string, prs []livePullRequest) (attention.Item, bool) {
	exact := make([]livePullRequest, 0, len(prs))
	for _, pr := range prs {
		if strings.EqualFold(strings.TrimSpace(pr.HeadRefOID), sha) {
			exact = append(exact, pr)
		}
	}
	sort.Slice(exact, func(i, j int) bool { return exact[i].Number < exact[j].Number })
	for _, pr := range exact {
		if strings.EqualFold(strings.TrimSpace(pr.State), "OPEN") {
			return attention.Item{
				Name: branch, Status: "ready-but-open", Level: attention.LevelCritical,
				SHA: sha, PullRequest: pr.Number,
				Reason: fmt.Sprintf("ready-but-open: canonical readiness=true for exact SHA %s; PR #%d is OPEN — merge is owed", sha, pr.Number),
			}, true
		}
	}
	if len(exact) > 0 {
		return attention.Item{}, false
	}
	name := branch
	if name == "" {
		name = sha
	}
	return attention.Item{
		Name: name, Status: "ready-without-pr", Level: attention.LevelCritical, SHA: sha,
		Reason: fmt.Sprintf("ready-without-pr: canonical readiness=true for exact SHA %s; no live PR identity matches this SHA — candidate is unlandable", sha),
	}, true
}

func ghPullRequestsForBranch(branch string) ([]livePullRequest, error) {
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "20", "--json", "number,state,headRefOid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list --head %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	var prs []livePullRequest
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list --head %s returned invalid JSON: %w", branch, err)
	}
	return prs, nil
}

func readinessBeatStatePath(ledgerPath string) string {
	return filepath.Join(filepath.Dir(ledgerPath), "attention-readiness-beats.json")
}

func loadReadinessBeatState(ledgerPath string) (readinessBeatState, error) {
	state := readinessBeatState{Schema: readinessBeatStateSchema, Conditions: map[string]int{}}
	body, err := os.ReadFile(readinessBeatStatePath(ledgerPath))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("herd-attention: read readiness beat state: %w", err)
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return state, fmt.Errorf("herd-attention: corrupt readiness beat state: %w", err)
	}
	if state.Schema != readinessBeatStateSchema || state.Conditions == nil {
		return state, fmt.Errorf("herd-attention: unsupported readiness beat state schema %d", state.Schema)
	}
	return state, nil
}

func saveReadinessBeatState(ledgerPath string, state readinessBeatState) error {
	path := readinessBeatStatePath(ledgerPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("herd-attention: create readiness state directory: %w", err)
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("herd-attention: encode readiness beat state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("herd-attention: write readiness beat state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("herd-attention: publish readiness beat state: %w", err)
	}
	return nil
}

func clearReadinessBeatState(ledgerPath string) error {
	return saveReadinessBeatState(ledgerPath, readinessBeatState{Schema: readinessBeatStateSchema, Conditions: map[string]int{}})
}
