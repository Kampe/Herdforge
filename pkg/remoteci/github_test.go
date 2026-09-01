package remoteci

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const liveShapedRunID int64 = 33463253256

func TestGitHubActionsResolvesRequiredJobNameNotWorkflowName(t *testing.T) {
	binding := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("b", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"Build, Preflight & Test Suite"},
	}
	runs := githubRunsJSON(binding, "CI Workflow", binding.Attempt, binding.CandidateSHA)
	jobs := githubJobsJSON(githubJob{
		RunID: liveShapedRunID, RunAttempt: binding.Attempt, Name: binding.RequiredChecks[0],
		HeadSHA: binding.CandidateSHA, Status: "completed", Conclusion: "success",
	})
	var calls int
	g := GitHubActions{Execute: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if name != "gh" {
			t.Fatalf("executable = %q, want gh", name)
		}
		switch calls {
		case 1:
			if !argumentsContainPair(args, "--repo", binding.Repository) || !argumentsContainPair(args, "--commit", binding.CandidateSHA) {
				t.Fatalf("run list lacks exact repository/SHA binding: %q", args)
			}
			return []byte(runs), nil
		case 2:
			wantEndpoint := "repos/Kampe/Herdforge/actions/runs/" + strconv.FormatInt(liveShapedRunID, 10) + "/attempts/1/jobs"
			if !argumentsContain(args, wantEndpoint) {
				t.Fatalf("jobs request lacks exact run/attempt endpoint: %q", args)
			}
			return []byte(jobs), nil
		default:
			t.Fatalf("unexpected provider call %d: %q", calls, args)
			return nil, nil
		}
	}}
	settlement, err := g.Watch(context.Background(), binding)
	if err != nil || settlement.State != StatePassed || !sameBinding(settlement.Binding, binding) {
		t.Fatalf("Watch = %+v, %v; want exact passed job settlement", settlement, err)
	}
	if calls != 2 {
		t.Fatalf("provider calls = %d, want run lookup plus attempt jobs lookup", calls)
	}
}

func TestGitHubActionsFailsClosedOnRunIdentity(t *testing.T) {
	binding := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("d", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}
	for name, tc := range map[string]struct {
		runs    string
		runErr  error
		wantErr error
	}{
		"unavailable":       {runErr: errors.New("no auth"), wantErr: ErrUnavailable},
		"no runs":           {runs: `[]`, wantErr: ErrNoChecks},
		"stale SHA":         {runs: githubRunsJSON(binding, "CI Workflow", 1, strings.Repeat("e", 40)), wantErr: ErrStale},
		"wrong attempt":     {runs: githubRunsJSON(binding, "CI Workflow", 2, binding.CandidateSHA), wantErr: ErrStale},
		"missing identity":  {runs: `[{"databaseId":0,"attempt":1,"name":"CI Workflow","headSha":"` + binding.CandidateSHA + `"}]`, wantErr: ErrAmbiguous},
		"duplicate matches": {runs: `[` + strings.Trim(githubRunsJSON(binding, "CI Workflow", 1, binding.CandidateSHA), "[]") + `,` + strings.Trim(githubRunsJSON(binding, "Other Workflow", 1, binding.CandidateSHA), "[]") + `]`, wantErr: ErrAmbiguous},
	} {
		t.Run(name, func(t *testing.T) {
			g := GitHubActions{Execute: func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tc.runs), tc.runErr
			}}
			_, err := g.Watch(context.Background(), binding)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Watch error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestGitHubActionsFailsClosedOnJobIdentityAndCompleteness(t *testing.T) {
	binding := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("9", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"build", "lint"},
	}
	build := githubJob{RunID: liveShapedRunID, RunAttempt: 1, Name: "build", HeadSHA: binding.CandidateSHA, Status: "completed", Conclusion: "success"}
	lint := githubJob{RunID: liveShapedRunID, RunAttempt: 1, Name: "lint", HeadSHA: binding.CandidateSHA, Status: "completed", Conclusion: "success"}
	for name, tc := range map[string]struct {
		jobs      string
		wantErr   error
		wantState State
	}{
		"missing required":   {jobs: githubJobsJSON(build), wantErr: ErrNoChecks},
		"duplicate required": {jobs: githubJobsJSON(build, lint, lint), wantErr: ErrAmbiguous},
		"partial page":       {jobs: `{"total_count":3,"jobs":` + jobsArrayJSON(build, lint) + `}`, wantErr: ErrAmbiguous},
		"wrong run":          {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.RunID++ })), wantErr: ErrAmbiguous},
		"wrong SHA":          {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.HeadSHA = strings.Repeat("8", 40) })), wantErr: ErrStale},
		"wrong attempt":      {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.RunAttempt = 2 })), wantErr: ErrStale},
		"pending":            {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.Status, j.Conclusion = "in_progress", "" })), wantErr: ErrPending},
		"unknown status":     {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.Status = "mystery" })), wantErr: ErrAmbiguous},
		"unknown conclusion": {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.Conclusion = "mystery" })), wantErr: ErrAmbiguous},
		"inconsistent state": {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.Status, j.Conclusion = "in_progress", "success" })), wantErr: ErrAmbiguous},
		"failed required":    {jobs: githubJobsJSON(build, withJob(lint, func(j *githubJob) { j.Conclusion = "failure" })), wantState: StateFailed},
		"failure beats pending": {
			jobs: githubJobsJSON(
				withJob(build, func(j *githubJob) { j.Status, j.Conclusion = "in_progress", "" }),
				withJob(lint, func(j *githubJob) { j.Conclusion = "failure" }),
			),
			wantState: StateFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := GitHubActions{Execute: githubSequenceExecutor(t,
				githubRunsJSON(binding, "CI Workflow", 1, binding.CandidateSHA), tc.jobs,
			)}
			settlement, err := g.Watch(context.Background(), binding)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Watch error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || settlement.State != tc.wantState {
				t.Fatalf("Watch = %+v, %v; want state %q", settlement, err, tc.wantState)
			}
		})
	}
}

func githubSequenceExecutor(t *testing.T, runs, jobs string) Executor {
	t.Helper()
	call := 0
	return func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		call++
		if call == 1 {
			return []byte(runs), nil
		}
		if call == 2 {
			return []byte(jobs), nil
		}
		t.Fatalf("unexpected provider call %d", call)
		return nil, nil
	}
}

func githubRunsJSON(binding Binding, name string, attempt int64, sha string) string {
	return fmt.Sprintf(`[{"databaseId":%d,"attempt":%d,"name":%q,"headSha":%q,"status":"completed","conclusion":"success"}]`, liveShapedRunID, attempt, name, sha)
}

func githubJobsJSON(jobs ...githubJob) string {
	return fmt.Sprintf(`{"total_count":%d,"jobs":%s}`, len(jobs), jobsArrayJSON(jobs...))
}

func jobsArrayJSON(jobs ...githubJob) string {
	parts := make([]string, 0, len(jobs))
	for _, job := range jobs {
		parts = append(parts, fmt.Sprintf(`{"run_id":%d,"run_attempt":%d,"name":%q,"head_sha":%q,"status":%q,"conclusion":%q}`, job.RunID, job.RunAttempt, job.Name, job.HeadSHA, job.Status, job.Conclusion))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func withJob(job githubJob, mutate func(*githubJob)) githubJob {
	mutate(&job)
	return job
}

func argumentsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func argumentsContain(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
