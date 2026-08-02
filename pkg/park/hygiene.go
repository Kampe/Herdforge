package park

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type HygieneRow struct {
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type HygieneResult struct {
	Rows       []HygieneRow `json:"rows"`
	Total      int          `json:"total"`
	Active     int          `json:"active"`
	Merged     int          `json:"merged"`
	Dup        int          `json:"dup"`
}

func Hygiene(ctx context.Context, repoRoot string) (*HygieneResult, error) {
	cmd := execCommandContext(ctx, "git", "tag", "-l", "parked/*", "--format", "%(refname:lstrip=2)%00%(objectname:short=7)%00%(*objectname:short=7)")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag -l parked/*: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return &HygieneResult{}, nil
	}

	var rows []HygieneRow
	mergedCount, dupCount, activeCount := 0, 0, 0
	seenCommits := make(map[string]bool)
	commits := make([]string, 0)

	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) < 3 {
			continue
		}
		tag, tagObj, targetObj := parts[0], parts[1], parts[2]

		commit := tagObj
		if targetObj != "" {
			commit = targetObj
		}

		commits = append(commits, commit)

		// Check if commit is reachable from HEAD
		reachMerge := isReachable(ctx, repoRoot, commit, "..HEAD")
		reachAncestor := isReachable(ctx, repoRoot, "HEAD", ".."+commit)

		status := "ACTIVE"
		reason := ""
		dup := seenCommits[commit]

		if reachMerge && !reachAncestor {
			status = "CONTENT_MERGED"
			reason = "commit is an ancestor of HEAD"
			mergedCount++
		} else if dup {
			status = "DUP"
			reason = "duplicate commit"
			dupCount++
		} else {
			activeCount++
		}

		rows = append(rows, HygieneRow{
			Tag:    tag,
			Commit: commit,
			Status: status,
			Reason: reason,
		})
		seenCommits[commit] = true
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Tag < rows[j].Tag
	})

	return &HygieneResult{
		Rows:   rows,
		Total:  len(rows),
		Active: activeCount,
		Merged: mergedCount,
		Dup:    dupCount,
	}, nil
}

func isReachable(ctx context.Context, repoRoot, base, path string) bool {
	cmd := execCommandContext(ctx, "git", "merge-base", "--is-ancestor", base+path)
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

func VerifyHygieneExit(result *HygieneResult) bool {
	return result.Dup == 0 && result.Merged == 0
}
