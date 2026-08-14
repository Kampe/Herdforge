package remoteci

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Executor is the sole process seam; production injects gh while tests never
// require it to be installed.
type Executor func(context.Context, string, ...string) ([]byte, error)

type GitHubActions struct{ Execute Executor }

type githubRun struct {
	HeadSHA    string `json:"headSha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func (g GitHubActions) Watch(ctx context.Context, binding Binding) (Settlement, error) {
	if err := binding.Validate(); err != nil {
		return Settlement{}, err
	}
	if g.Execute == nil {
		return Settlement{}, fmt.Errorf("%w: gh executor is not configured", ErrUnavailable)
	}
	out, err := g.Execute(ctx, "gh", "run", "list", "--repo", binding.Repository, "--commit", binding.CandidateSHA, "--json", "headSha,status,conclusion", "--limit", "100")
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: gh run list: %v", ErrUnavailable, err)
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return Settlement{}, fmt.Errorf("%w: gh JSON: %v", ErrUnavailable, err)
	}
	if len(runs) == 0 {
		return Settlement{}, ErrNoChecks
	}
	for _, run := range runs {
		if run.HeadSHA != binding.CandidateSHA {
			return Settlement{}, fmt.Errorf("%w: gh returned a run for a different candidate", ErrStale)
		}
		if strings.EqualFold(run.Status, "queued") || strings.EqualFold(run.Status, "in_progress") || strings.TrimSpace(run.Conclusion) == "" {
			return Settlement{Version: Version1, Binding: binding, State: StatePending}, ErrPending
		}
		if !strings.EqualFold(run.Conclusion, "success") && !strings.EqualFold(run.Conclusion, "neutral") {
			return Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: redactBounded("GitHub Actions conclusion: " + run.Conclusion)}, nil
		}
	}
	return Settlement{Version: Version1, Binding: binding, State: StatePassed}, nil
}
