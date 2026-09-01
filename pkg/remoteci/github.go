package remoteci

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Executor is the sole process seam; production injects gh while tests never
// require it to be installed.
type Executor func(context.Context, string, ...string) ([]byte, error)

type GitHubActions struct{ Execute Executor }

type githubRun struct {
	DatabaseID int64  `json:"databaseId"`
	Name       string `json:"name"`
	HeadSHA    string `json:"headSha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int64  `json:"attempt"`
}

type githubJobs struct {
	TotalCount int         `json:"total_count"`
	Jobs       []githubJob `json:"jobs"`
}

type githubJob struct {
	RunID      int64  `json:"run_id"`
	RunAttempt int64  `json:"run_attempt"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

var githubRepositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (g GitHubActions) Watch(ctx context.Context, binding Binding) (Settlement, error) {
	if err := binding.Validate(); err != nil {
		return Settlement{}, err
	}
	if g.Execute == nil {
		return Settlement{}, fmt.Errorf("%w: gh executor is not configured", ErrUnavailable)
	}
	out, err := g.Execute(ctx, "gh", "run", "list", "--repo", binding.Repository, "--commit", binding.CandidateSHA, "--json", "databaseId,attempt,name,headSha,status,conclusion", "--limit", "100")
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: gh run list: %v", ErrUnavailable, err)
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return Settlement{}, fmt.Errorf("%w: gh run JSON: %v", ErrUnavailable, err)
	}
	if len(runs) == 0 {
		return Settlement{}, ErrNoChecks
	}
	exactRuns := make([]githubRun, 0, 1)
	for _, run := range runs {
		if run.HeadSHA != binding.CandidateSHA {
			return Settlement{}, fmt.Errorf("%w: gh returned a workflow run for a different candidate", ErrStale)
		}
		if run.Attempt < 1 || run.DatabaseID < 1 {
			return Settlement{}, fmt.Errorf("%w: workflow run omitted database or attempt identity", ErrAmbiguous)
		}
		if run.Attempt == binding.Attempt {
			exactRuns = append(exactRuns, run)
		}
	}
	if len(exactRuns) == 0 {
		return Settlement{}, fmt.Errorf("%w: no workflow run reported attempt %d", ErrStale, binding.Attempt)
	}
	if len(exactRuns) != 1 {
		return Settlement{}, fmt.Errorf("%w: %d workflow runs reported candidate attempt %d", ErrAmbiguous, len(exactRuns), binding.Attempt)
	}
	run := exactRuns[0]
	endpoint, err := GitHubAttemptJobsEndpoint(binding.Repository, run.DatabaseID, binding.Attempt)
	if err != nil {
		return Settlement{}, err
	}
	out, err = g.Execute(ctx, "gh", "api", "--method", "GET", endpoint, "-f", "per_page=100")
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: gh run jobs: %v", ErrUnavailable, err)
	}
	var response githubJobs
	if err := json.Unmarshal(out, &response); err != nil {
		return Settlement{}, fmt.Errorf("%w: gh jobs JSON: %v", ErrUnavailable, err)
	}
	if response.TotalCount < 0 || response.TotalCount != len(response.Jobs) {
		return Settlement{}, fmt.Errorf("%w: jobs response is incomplete (%d of %d)", ErrAmbiguous, len(response.Jobs), response.TotalCount)
	}
	if len(response.Jobs) == 0 {
		return Settlement{}, ErrNoChecks
	}
	byName := make(map[string][]githubJob, len(response.Jobs))
	for _, job := range response.Jobs {
		if job.RunID != run.DatabaseID {
			return Settlement{}, fmt.Errorf("%w: job belongs to workflow run %d instead of %d", ErrAmbiguous, job.RunID, run.DatabaseID)
		}
		if job.HeadSHA != binding.CandidateSHA || job.RunAttempt != binding.Attempt {
			return Settlement{}, fmt.Errorf("%w: job candidate or attempt differs from the registered watch", ErrStale)
		}
		name := strings.TrimSpace(job.Name)
		if name != "" {
			key := strings.ToLower(name)
			byName[key] = append(byName[key], job)
		}
	}
	outcomes := make([]State, 0, len(binding.RequiredChecks))
	for _, required := range binding.RequiredChecks {
		jobs := byName[strings.ToLower(strings.TrimSpace(required))]
		if len(jobs) == 0 {
			return Settlement{}, fmt.Errorf("%w: required check job %q was not reported", ErrNoChecks, required)
		}
		if len(jobs) != 1 {
			return Settlement{}, fmt.Errorf("%w: required check job %q was reported %d times", ErrAmbiguous, required, len(jobs))
		}
		outcome, err := githubJobOutcome(jobs[0])
		if err != nil {
			return Settlement{}, fmt.Errorf("required check job %q: %w", required, err)
		}
		outcomes = append(outcomes, outcome)
	}
	for i, outcome := range outcomes {
		if outcome == StateFailed {
			required := binding.RequiredChecks[i]
			job := byName[strings.ToLower(strings.TrimSpace(required))][0]
			return Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: redactBounded("GitHub Actions required check " + required + " conclusion: " + job.Conclusion)}, nil
		}
	}
	for _, outcome := range outcomes {
		if outcome == StatePending {
			return Settlement{Version: Version1, Binding: binding, State: StatePending}, ErrPending
		}
	}
	return Settlement{Version: Version1, Binding: binding, State: StatePassed}, nil
}

func githubJobOutcome(job githubJob) (State, error) {
	status := strings.ToLower(strings.TrimSpace(job.Status))
	conclusion := strings.ToLower(strings.TrimSpace(job.Conclusion))
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		if conclusion != "" {
			return "", fmt.Errorf("%w: nonterminal status %q has conclusion %q", ErrAmbiguous, job.Status, job.Conclusion)
		}
		return StatePending, nil
	case "completed":
		switch conclusion {
		case "success", "neutral":
			return StatePassed, nil
		case "failure", "cancelled", "timed_out", "action_required", "stale", "skipped", "startup_failure":
			return StateFailed, nil
		case "":
			return "", fmt.Errorf("%w: completed job has no conclusion", ErrAmbiguous)
		default:
			return "", fmt.Errorf("%w: unknown completed conclusion %q", ErrAmbiguous, job.Conclusion)
		}
	default:
		return "", fmt.Errorf("%w: unknown job status %q", ErrAmbiguous, job.Status)
	}
}

// GitHubAttemptJobsEndpoint returns the attempt-scoped job surface used by
// both production observation and executable test fixtures.
func GitHubAttemptJobsEndpoint(repository string, runID, attempt int64) (string, error) {
	if runID < 1 || attempt < 1 {
		return "", fmt.Errorf("%w: positive GitHub run and attempt identities are required", ErrInvalid)
	}
	slug, err := githubRepositorySlug(repository)
	if err != nil {
		return "", err
	}
	return "repos/" + slug + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/attempts/" + strconv.FormatInt(attempt, 10) + "/jobs", nil
}

func githubRepositorySlug(repository string) (string, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	repository = strings.TrimPrefix(repository, "https://")
	repository = strings.TrimPrefix(repository, "http://")
	repository = strings.TrimPrefix(repository, "github.com/")
	repository = strings.TrimSuffix(repository, ".git")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !githubRepositoryPart.MatchString(parts[0]) || !githubRepositoryPart.MatchString(parts[1]) {
		return "", fmt.Errorf("%w: repository %q is not an exact GitHub owner/name", ErrInvalid, repository)
	}
	return parts[0] + "/" + parts[1], nil
}
