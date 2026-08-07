package park

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var chaTicketRe = regexp.MustCompile(`(?i)cha-[0-9]+`)

type HygieneRow struct {
	Flag    string `json:"flag"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
	Age     string `json:"age"`
	Subject string `json:"subject"`
}

type HygieneResult struct {
	Rows          []HygieneRow `json:"rows"`
	DupClusters   []string     `json:"dup_clusters,omitempty"`
	ContentMerged int          `json:"content_merged"`
	Dup           int          `json:"dup"`
	Total         int          `json:"total"`
}

// Hygiene classifies every park/* and parked/* branch tip: CONTENT_MERGED
// when its subject already landed on origin/main, or ACTIVE otherwise; and
// flags CHA-<n> ticket clusters with more than one branch tip as duplicates.
// It never deletes anything — cleanup is delegated to Reap.
func Hygiene(ctx context.Context, repoRoot string) (*HygieneResult, error) {
	onMain, err := mainSubjects(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	branches, err := parkBranches(ctx, repoRoot)
	if err != nil {
		return nil, err
	}

	var rows []HygieneRow
	contentMerged := 0
	tipsByTicket := map[string][]string{}

	for _, b := range branches {
		sha, subject, age, err := branchTip(ctx, repoRoot, b)
		if err != nil {
			return nil, err
		}

		flag := "ACTIVE"
		if onMain[subject] {
			flag = "CONTENT_MERGED"
			contentMerged++
		}
		rows = append(rows, HygieneRow{Flag: flag, Branch: b, SHA: sha, Age: age, Subject: subject})

		if m := chaTicketRe.FindString(subject); m != "" {
			ticket := strings.ToUpper(m)
			tipsByTicket[ticket] = append(tipsByTicket[ticket], b)
		}
	}

	var tickets []string
	for t := range tipsByTicket {
		tickets = append(tickets, t)
	}
	sort.Strings(tickets)

	var dupClusters []string
	dup := 0
	for _, t := range tickets {
		tips := tipsByTicket[t]
		if len(tips) <= 1 {
			continue
		}
		sort.Strings(tips)
		dupClusters = append(dupClusters, fmt.Sprintf("%s -> %d tips: %s", t, len(tips), strings.Join(tips, ", ")))
		dup += len(tips)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Branch < rows[j].Branch })

	return &HygieneResult{
		Rows:          rows,
		DupClusters:   dupClusters,
		ContentMerged: contentMerged,
		Dup:           dup,
		Total:         len(rows),
	}, nil
}

// VerifyHygieneExit reports whether the fleet is clean of dup CHA clusters
// and content-merged park tips. false means the exit code must be non-zero.
func VerifyHygieneExit(result *HygieneResult) bool {
	return result.Dup == 0 && result.ContentMerged == 0
}

// mainSubjects builds the set of commit subjects already on origin/main. A
// repo with no origin/main yet (fresh init, no remote) simply has nothing
// merged — that is a classification input, not a fatal error.
func mainSubjects(ctx context.Context, repoRoot string) (map[string]bool, error) {
	cmd := execCommandContext(ctx, "git", "log", "origin/main", "--format=%s")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return map[string]bool{}, nil
	}
	set := map[string]bool{}
	for _, line := range splitNonEmpty(string(out)) {
		set[line] = true
	}
	return set, nil
}

func parkBranches(ctx context.Context, repoRoot string) ([]string, error) {
	branches, err := localBranches(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range branches {
		if strings.HasPrefix(b, "park/") || strings.HasPrefix(b, "parked/") {
			out = append(out, b)
		}
	}
	return out, nil
}

func branchTip(ctx context.Context, repoRoot, branch string) (sha, subject, age string, err error) {
	cmd := execCommandContext(ctx, "git", "log", "-1", "--format=%h%x00%s%x00%cd", "--date=short", branch)
	cmd.Dir = repoRoot
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", "", "", fmt.Errorf("git log -1 %s: %w", branch, runErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("git log -1 %s: unexpected output %q", branch, out)
	}
	return parts[0], parts[1], parts[2], nil
}
