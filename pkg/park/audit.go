package park

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var parkedSubjectRe = regexp.MustCompile(`^(wip\(parked\)|wip:|parked:)`)

type Durability int

const (
	Exposed Durability = iota
	LocalTagOnly
	Durable
)

func (d Durability) String() string {
	switch d {
	case Durable:
		return "DURABLE"
	case LocalTagOnly:
		return "LOCAL-TAG-ONLY"
	case Exposed:
		return "EXPOSED"
	}
	return "UNKNOWN"
}

type AuditEntry struct {
	SHA        string     `json:"sha"`
	Branch     string     `json:"branch"`
	Subject    string     `json:"subject"`
	Tags       []string   `json:"tags,omitempty"`
	Durability Durability `json:"durability"`
}

type AuditResult struct {
	Entries    []AuditEntry `json:"entries"`
	Durable    int          `json:"durable"`
	NotDurable int          `json:"not_durable"`
	Total      int          `json:"total"`
}

// Audit finds every wip(parked):/wip:/parked: commit reachable only from a
// local branch (origin/main..branch) and classifies its durability: DURABLE
// (tagged, and the tag is pushed to origin), LOCAL-TAG-ONLY (tagged, not
// pushed), or EXPOSED (no tag at all — reachable only from the branch, so a
// reset --hard + gc loses it for good).
func Audit(ctx context.Context, repoRoot string) (*AuditResult, error) {
	branches, err := localBranches(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	remoteTags, err := remoteTagSet(ctx, repoRoot)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var entries []AuditEntry
	durable, notDurable := 0, 0

	for _, branch := range branches {
		if branch == "main" {
			continue
		}
		cmd := execCommandContext(ctx, "git", "log", "--format=%H%x00%s", "origin/main.."+branch)
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, line := range splitNonEmpty(string(out)) {
			parts := strings.SplitN(line, "\x00", 2)
			if len(parts) != 2 {
				continue
			}
			sha, subject := parts[0], parts[1]
			if !parkedSubjectRe.MatchString(subject) || seen[sha] {
				continue
			}
			seen[sha] = true

			tags, err := tagsContaining(ctx, repoRoot, sha)
			if err != nil {
				return nil, err
			}

			entry := AuditEntry{SHA: sha, Branch: branch, Subject: subject, Tags: tags}
			switch {
			case len(tags) == 0:
				entry.Durability = Exposed
				notDurable++
			case anyTagOn(tags, remoteTags):
				entry.Durability = Durable
				durable++
			default:
				entry.Durability = LocalTagOnly
				notDurable++
			}
			entries = append(entries, entry)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].SHA < entries[j].SHA })

	return &AuditResult{Entries: entries, Durable: durable, NotDurable: notDurable, Total: len(entries)}, nil
}

// VerifyAuditExit reports whether every parked commit found is durable (tag
// pushed to origin). false means the audit's exit code must be non-zero.
func VerifyAuditExit(result *AuditResult) bool {
	return result.NotDurable == 0
}

func localBranches(ctx context.Context, repoRoot string) ([]string, error) {
	cmd := execCommandContext(ctx, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref refs/heads/: %w", err)
	}
	return splitNonEmpty(string(out)), nil
}

// remoteTagSet lists tags present on origin. A repo without an origin remote
// simply has no durable tags yet — that is a classification input, not a
// fatal error.
func remoteTagSet(ctx context.Context, repoRoot string) (map[string]bool, error) {
	cmd := execCommandContext(ctx, "git", "ls-remote", "--tags", "origin")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return map[string]bool{}, nil
	}
	set := map[string]bool{}
	for _, line := range splitNonEmpty(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := strings.TrimSuffix(fields[1], "^{}")
		set[strings.TrimPrefix(ref, "refs/tags/")] = true
	}
	return set, nil
}

func tagsContaining(ctx context.Context, repoRoot, sha string) ([]string, error) {
	cmd := execCommandContext(ctx, "git", "for-each-ref", "--contains", sha, "--format=%(refname:short)", "refs/tags/")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref --contains %s: %w", sha, err)
	}
	return splitNonEmpty(string(out)), nil
}

func anyTagOn(tags []string, remote map[string]bool) bool {
	for _, t := range tags {
		if remote[t] {
			return true
		}
	}
	return false
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
