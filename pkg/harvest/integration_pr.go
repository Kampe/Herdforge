package harvest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// IntegrationPRBinding pins the destination independently of cwd, the focused
// GitHub repository, and a moving local branch. Candidate is the reviewed
// identity; Head is the harvested identity. Their content relationship belongs
// to the native admission/proof gates, not to the PR publisher.
type IntegrationPRBinding struct {
	Repository string `json:"repository"` // HOST/OWNER/REPO
	Candidate  string `json:"candidate"`
	Head       string `json:"head"`
	Branch     string `json:"branch"`
	Base       string `json:"base"`
}

// IntegrationPR is provider readback. It is evidence of a PR, not review,
// merge permission, a runtime deployment, or a completion receipt.
type IntegrationPR struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	URL         string `json:"url"`
	Head        string `json:"headRefOid"`
	Branch      string `json:"headRefName"`
	Base        string `json:"baseRefName"`
	CrossRepo   *bool  `json:"isCrossRepository"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	MergedAt string `json:"mergedAt"`
}

// IntegrationPRPublisher performs only the integration-pr stage. In
// particular Publish never merges, closes a PR, deletes a branch, or updates
// main. The caller must have passed the native review/harvest/capacity gates.
type IntegrationPRPublisher struct {
	RepoRoot string
	Binding  IntegrationPRBinding
	command  func(context.Context, string, []string, string) ([]byte, error)
}

var integrationRepoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (p *IntegrationPRPublisher) validate(ctx context.Context) error {
	b := p.Binding
	parts := strings.Split(b.Repository, "/")
	if len(parts) != 3 {
		return fmt.Errorf("integration PR: repository must be explicit HOST/OWNER/REPO")
	}
	for _, part := range parts {
		if !integrationRepoPart.MatchString(part) || part == "." || part == ".." {
			return fmt.Errorf("integration PR: invalid repository identity")
		}
	}
	if !fullIntegrationSHA(b.Candidate) || !fullIntegrationSHA(b.Head) {
		return fmt.Errorf("integration PR: full candidate and harvested head required")
	}
	if !strings.HasPrefix(b.Branch, IntegrationNamespace) || b.Branch == b.Base || b.Base == "" {
		return fmt.Errorf("integration PR: separate integration branch and explicit target required")
	}
	for _, branch := range []string{b.Branch, b.Base} {
		if _, err := p.run(ctx, 15*time.Second, "git", []string{"check-ref-format", "refs/heads/" + branch}, ""); err != nil {
			return fmt.Errorf("integration PR: invalid branch: %w", err)
		}
	}
	// A configured pushurl may differ from the fetch URL, or fan out to more
	// than one host. Check BOTH before any GitHub or push operation.
	for _, push := range []bool{false, true} {
		args := []string{"remote", "get-url", "--all"}
		if push {
			args = append(args, "--push")
		}
		args = append(args, "origin")
		raw, err := p.run(ctx, 15*time.Second, "git", args, "")
		if err != nil {
			return fmt.Errorf("integration PR: remote identity UNKNOWN: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 1 {
			return fmt.Errorf("integration PR: multiple remote destinations refused")
		}
		normalized, ok := normalizeRemoteIdentity(lines[0])
		if !ok {
			return fmt.Errorf("integration PR: remote has no hosted repository identity")
		}
		u, err := url.Parse(normalized)
		if err != nil || u.Host == "" {
			return fmt.Errorf("integration PR: invalid hosted repository URL")
		}
		repository := u.Host + "/" + strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		if !strings.EqualFold(repository, b.Repository) {
			return fmt.Errorf("integration PR: remote repository does not match pinned destination")
		}
	}
	return nil
}

// Observe distinguishes a successful empty query from unknown/failed reads.
// A nil PR with nil error means the exact branch has no PR. A closed PR,
// duplicate branch identity, foreign head, or malformed response refuses.
func (p *IntegrationPRPublisher) Observe(ctx context.Context) (*IntegrationPR, error) {
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	return p.observe(ctx)
}

func (p *IntegrationPRPublisher) observe(ctx context.Context) (*IntegrationPR, error) {
	fields := "number,state,url,headRefOid,headRefName,baseRefName,isCrossRepository,mergeCommit,mergedAt"
	out, err := p.run(ctx, 15*time.Second, "gh", []string{"pr", "list", "--repo", p.Binding.Repository, "--head", p.Binding.Branch, "--state", "all", "--limit", "2", "--json", fields}, "")
	if err != nil {
		return nil, fmt.Errorf("integration PR lookup UNKNOWN: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "[") {
		return nil, fmt.Errorf("integration PR lookup UNKNOWN: expected an explicit result array")
	}
	var prs []IntegrationPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("integration PR lookup UNKNOWN: invalid response: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	if len(prs) != 1 {
		return nil, fmt.Errorf("integration PR lookup ambiguous: multiple PRs name the branch")
	}
	pr := &prs[0]
	if err := p.validatePR(pr); err != nil {
		return nil, err
	}
	return pr, nil
}

func (p *IntegrationPRPublisher) validatePR(pr *IntegrationPR) error {
	b := p.Binding
	if pr.Number <= 0 || pr.Head != b.Head || pr.Branch != b.Branch || pr.Base != b.Base || pr.CrossRepo == nil || *pr.CrossRepo {
		return fmt.Errorf("integration PR: provider result does not match exact same-repository head/base identity")
	}
	u, err := url.Parse(pr.URL)
	want := strings.Split(b.Repository, "/")
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, want[0]) || !strings.EqualFold(u.Path, "/"+want[1]+"/"+want[2]+"/pull/"+strconv.Itoa(pr.Number)) || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("integration PR: provider URL does not match repository and PR number")
	}
	switch pr.State {
	case "OPEN":
		if pr.MergedAt != "" || pr.MergeCommit.OID != "" {
			return fmt.Errorf("integration PR: open result carries contradictory merge evidence")
		}
	case "MERGED":
		if !fullIntegrationSHA(pr.MergeCommit.OID) {
			return fmt.Errorf("integration PR: merged result has no exact merge commit")
		}
		if _, err := time.Parse(time.RFC3339, pr.MergedAt); err != nil {
			return fmt.Errorf("integration PR: merged result has no valid merge timestamp")
		}
	default:
		return fmt.Errorf("integration PR: state %q does not admit publication or merge recovery", pr.State)
	}
	return nil
}

// Publish resumes safely after either half of publication: an existing exact
// branch avoids another push; an existing exact PR avoids another create. An
// uncertain producer exit is returned as an error, never retried in this call.
func (p *IntegrationPRPublisher) Publish(ctx context.Context, title, body string) (*IntegrationPR, error) {
	if strings.TrimSpace(title) == "" || strings.ContainsAny(title, "\r\n") || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("integration PR: title and retained evidence body are required")
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	pr, err := p.observe(ctx)
	if err != nil || pr != nil {
		return pr, err
	}
	if _, err := p.run(ctx, 15*time.Second, "git", []string{"cat-file", "-e", p.Binding.Head + "^{commit}"}, ""); err != nil {
		return nil, fmt.Errorf("integration PR: harvested commit is unavailable: %w", err)
	}
	ref := "refs/heads/" + p.Binding.Branch
	out, err := p.run(ctx, 15*time.Second, "git", []string{"ls-remote", "--heads", "--refs", "origin", ref}, "")
	if err != nil {
		return nil, fmt.Errorf("integration PR: remote branch UNKNOWN: %w", err)
	}
	remote := strings.Fields(string(out))
	switch len(remote) {
	case 0:
		// Empty expected old value is a create-only CAS. It cannot replace
		// an existing branch, even if another publisher races this read.
		if _, err := p.run(ctx, 2*time.Minute, "git", []string{"push", gitroot.RefLeaseFlagPrefix + ref + ":", "origin", p.Binding.Head + ":" + ref}, ""); err != nil {
			return nil, fmt.Errorf("integration PR: branch publication outcome requires readback: %w", err)
		}
	case 2:
		if remote[0] != p.Binding.Head || remote[1] != ref {
			return nil, fmt.Errorf("integration PR: remote branch drift; refusing to overwrite")
		}
	default:
		return nil, fmt.Errorf("integration PR: remote branch readback is ambiguous")
	}
	// Prompt/evidence text is stdin data, never shell source or an argv body.
	if _, err := p.run(ctx, 30*time.Second, "gh", []string{"pr", "create", "--repo", p.Binding.Repository, "--head", p.Binding.Branch, "--base", p.Binding.Base, "--title", title, "--body-file", "-"}, body); err != nil {
		return nil, fmt.Errorf("integration PR: creation outcome requires readback: %w", err)
	}
	pr, err = p.observe(ctx)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("integration PR: successful create had no exact provider readback")
	}
	return pr, nil
}

func (p *IntegrationPRPublisher) run(ctx context.Context, limit time.Duration, name string, args []string, input string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.command != nil {
		return p.command(ctx, name, args, input)
	}
	cmd := execCommandContext(ctx, name, args...)
	cmd.Dir = p.RepoRoot
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w", name, args[0], err)
	}
	return out, nil
}

func fullIntegrationSHA(sha string) bool {
	return len(sha) == 40 && strings.Trim(sha, "0123456789abcdef") == ""
}
