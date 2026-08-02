package park

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Durability int

const (
	LocalTagOnly Durability = iota
	Durable
	Exposed
)

func (d Durability) String() string {
	switch d {
	case LocalTagOnly:
		return "LOCAL-TAG-ONLY"
	case Durable:
		return "DURABLE"
	case Exposed:
		return "EXPOSED"
	}
	return "UNKNOWN"
}

type AuditEntry struct {
	Tag        string     `json:"tag"`
	Commit     string     `json:"commit"`
	Message    string     `json:"message"`
	Date       string     `json:"date"`
	Durability Durability `json:"durability"`
}

type AuditResult struct {
	Entries  []AuditEntry `json:"entries"`
	Local    int          `json:"local"`
	Durable  int          `json:"durable"`
	Exposed  int          `json:"exposed"`
	Total    int          `json:"total"`
}

func Audit(ctx context.Context, repoRoot string) (*AuditResult, error) {
	cmd := execCommandContext(ctx, "git", "tag", "-l", "parked/*", "--format", "%(objecttype)%00%(refname:lstrip=2)%00%(objectname:short=7)%00%(subject)%00%(creatordate:short)")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag -l parked/*: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return &AuditResult{}, nil
	}

	var entries []AuditEntry
	localCount, durableCount, exposedCount := 0, 0, 0

	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, "\x00", 5)
		if len(parts) < 5 {
			continue
		}
		objType, tagName, commit, msg, date := parts[0], parts[1], parts[2], parts[3], parts[4]

		if objType != "tag" && strings.TrimSpace(objType) != "" {
			continue
		}

		entry := AuditEntry{
			Tag:     tagName,
			Commit:  commit,
			Message: msg,
			Date:    date,
		}

		hasRemote := remoteTagExists(ctx, repoRoot, tagName)
		if hasRemote {
			entry.Durability = Exposed
			exposedCount++
		} else {
			entry.Durability = LocalTagOnly
			localCount++
		}

		if _, err := time.Parse("2006-01-02", date); err != nil {
			entry.Date = "unknown"
		}

		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Date != entries[j].Date {
			return entries[i].Date > entries[j].Date
		}
		return entries[i].Tag < entries[j].Tag
	})

	return &AuditResult{
		Entries:  entries,
		Local:    localCount,
		Durable:  durableCount,
		Exposed:  exposedCount,
		Total:    len(entries),
	}, nil
}

func remoteTagExists(ctx context.Context, repoRoot, tag string) bool {
	cmd := execCommandContext(ctx, "git", "ls-remote", "--tags", "origin", "refs/tags/"+tag)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func VerifyAuditExit(result *AuditResult) bool {
	return result.Exposed == 0
}
