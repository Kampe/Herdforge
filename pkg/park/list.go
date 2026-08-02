package park

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ParkedCommit struct {
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
	Message string `json:"message"`
	Date    string `json:"date"`
}

type ListOptions struct {
	Author *string
	Since  *string
}

type ListResult struct {
	Commits []ParkedCommit `json:"commits"`
	Total   int            `json:"total"`
}

func List(ctx context.Context, repoRoot string, opts ListOptions) (*ListResult, error) {
	args := []string{"tag", "-l", "parked/*", "--format", "%(refname:lstrip=2)%00%(objectname:short=7)%00%(subject)%00%(creatordate:short)", "--sort", "-creatordate"}
	cmd := execCommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag -l parked/*: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return &ListResult{}, nil
	}

	var commits []ParkedCommit
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, "\x00", 4)
		if len(parts) < 4 {
			continue
		}
		tagName, commit, msg, date := parts[0], parts[1], parts[2], parts[3]

		if _, err := time.Parse("2006-01-02", date); err != nil {
			date = "unknown"
		}
		if strings.TrimSpace(tagName) == "" {
			continue
		}

		c := ParkedCommit{
			Tag:     tagName,
			Commit:  commit,
			Message: msg,
			Date:    date,
		}

		if opts.Author != nil {
			continue
		}
		if opts.Since != nil {
			continue
		}

		commits = append(commits, c)
	}

	return &ListResult{
		Commits: commits,
		Total:   len(commits),
	}, nil
}
