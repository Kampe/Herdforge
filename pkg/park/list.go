package park

import (
	"context"
	"fmt"
	"strings"
)

type ParkedCommit struct {
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
	Message string `json:"message"`
}

type ListResult struct {
	Commits []ParkedCommit `json:"commits"`
	Total   int            `json:"total"`
}

// List shows every parked tag with its short object id and the subject of
// the commit it points to (not the tag's own -m annotation).
func List(ctx context.Context, repoRoot string) (*ListResult, error) {
	cmd := execCommandContext(ctx, "git", "tag", "-l", "parked/*", "--format", "%(refname:lstrip=2)%00%(objectname:short)")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag -l parked/*: %w", err)
	}

	var commits []ParkedCommit
	for _, line := range splitNonEmpty(string(out)) {
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		tagName, shortSHA := parts[0], parts[1]

		subject, err := commitSubject(ctx, repoRoot, tagName+"^{commit}")
		if err != nil {
			subject = ""
		}

		commits = append(commits, ParkedCommit{Tag: tagName, Commit: shortSHA, Message: subject})
	}

	return &ListResult{Commits: commits, Total: len(commits)}, nil
}

func commitSubject(ctx context.Context, repoRoot, ref string) (string, error) {
	cmd := execCommandContext(ctx, "git", "log", "-1", "--format=%s", ref)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
