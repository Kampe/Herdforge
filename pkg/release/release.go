package release

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type ReleaseNotes struct {
	Version   string
	Date      time.Time
	Features  []string
	Fixes     []string
	Refactors []string
}

type ReleaseEngine struct {
	RepoRoot string
}

func NewReleaseEngine(repoRoot string) *ReleaseEngine {
	return &ReleaseEngine{RepoRoot: repoRoot}
}

// GenerateChangelog parses conventional git commit history into formatted Markdown changelog
func (r *ReleaseEngine) GenerateChangelog(ctx context.Context, fromTag, version string) (*ReleaseNotes, string, error) {
	args := []string{"log", "--oneline", "--no-merges"}
	if fromTag != "" {
		args = append(args, fmt.Sprintf("%s..HEAD", fromTag))
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.RepoRoot

	out, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch git commit log: %w", err)
	}

	notes := &ReleaseNotes{
		Version: version,
		Date:    time.Now(),
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		msg := parts[1]

		switch {
		case strings.HasPrefix(msg, "feat"):
			notes.Features = append(notes.Features, msg)
		case strings.HasPrefix(msg, "fix"):
			notes.Fixes = append(notes.Fixes, msg)
		case strings.HasPrefix(msg, "refactor"):
			notes.Refactors = append(notes.Refactors, msg)
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Release %s (%s)\n\n", version, notes.Date.Format("2006-01-02")))

	if len(notes.Features) > 0 {
		sb.WriteString("### 🚀 Features\n")
		for _, f := range notes.Features {
			sb.WriteString(fmt.Sprintf("- %s\n", f))
		}
		sb.WriteString("\n")
	}

	if len(notes.Fixes) > 0 {
		sb.WriteString("### 🐛 Bug Fixes\n")
		for _, f := range notes.Fixes {
			sb.WriteString(fmt.Sprintf("- %s\n", f))
		}
		sb.WriteString("\n")
	}

	if len(notes.Refactors) > 0 {
		sb.WriteString("### 🧹 Refactoring & Maintenance\n")
		for _, r := range notes.Refactors {
			sb.WriteString(fmt.Sprintf("- %s\n", r))
		}
		sb.WriteString("\n")
	}

	return notes, sb.String(), nil
}
