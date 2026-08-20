package containerlifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// VerifyHarnessLabel is the durable ownership marker applied to every
// ephemeral compose stack created by the verification harness.
const VerifyHarnessLabel = "com.herdforge.verify-harness"

// VerifyRunStateLabel is written by the harness and is authoritative only
// when it is present. A missing label is blocked, never treated as finished.
const VerifyRunStateLabel = "com.herdforge.verify-run-state"

const (
	VerifyRunLive     = "live"
	VerifyRunFinished = "finished"
	VerifyRunFailed   = "failed"
)

// ComposeStack is the ownership/liveness projection needed by cleanup. A
// missing ownership or run-state fact is intentionally distinguishable from
// false: callers must leave such a stack alone.
type ComposeStack struct {
	Project        string
	WorkingDir     string
	VerifyHarness  bool
	VerifyRunState string
	CreatedAt      time.Time
}

// DockerComposeStackClient discovers only containers carrying the durable
// verify-harness label, then groups them by Compose project. It never uses a
// project-name prefix as an ownership test.
type DockerComposeStackClient struct{}

func NewDockerComposeStackClient() DockerComposeStackClient { return DockerComposeStackClient{} }

func (DockerComposeStackClient) List(ctx context.Context) ([]ComposeStack, error) {
	format := "{{.Label \"com.docker.compose.project\"}}\t{{.Label \"com.docker.compose.project.working_dir\"}}\t{{.Label \"com.herdforge.verify-run-state\"}}\t{{.CreatedAt}}\t{{.State}}"
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label="+VerifyHarnessLabel, "--format", format)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps verify stacks: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	byProject := map[string]ComposeStack{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			return nil, fmt.Errorf("docker ps verify stacks: malformed record")
		}
		project, workingDir, runState, createdRaw, state := parts[0], parts[1], parts[2], parts[3], parts[4]
		created, parseErr := parseDockerCreatedAt(createdRaw)
		if parseErr != nil {
			created = time.Time{}
		}
		stack := byProject[project]
		stack.Project = project
		stack.WorkingDir = workingDir
		stack.VerifyHarness = true
		if stack.VerifyRunState == "" {
			stack.VerifyRunState = runState
		} else if stack.VerifyRunState != runState {
			stack.VerifyRunState = ""
		}
		if stack.CreatedAt.IsZero() || (!created.IsZero() && created.Before(stack.CreatedAt)) {
			stack.CreatedAt = created
		}
		if state == "running" {
			stack.VerifyRunState = VerifyRunLive
		}
		byProject[project] = stack
	}
	stacks := make([]ComposeStack, 0, len(byProject))
	for _, stack := range byProject {
		stacks = append(stacks, stack)
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Project < stacks[j].Project })
	return stacks, nil
}

func parseDockerCreatedAt(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05 -0700"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Docker created-at timestamp %q", raw)
}

func (DockerComposeStackClient) Down(ctx context.Context, project, workingDir string) error {
	if project == "" || workingDir == "" {
		return fmt.Errorf("compose cleanup: project and working directory are required")
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "--project-name", project, "--project-directory", workingDir, "down", "--remove-orphans")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose down %s: %w: %s", project, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type ComposeStackClient interface {
	List(context.Context) ([]ComposeStack, error)
	Down(ctx context.Context, project, workingDir string) error
}

type ReapVerifyStacksOptions struct {
	MaxAge time.Duration
	DryRun bool
	Now    func() time.Time
}

type ReapVerifyStacksReport struct {
	DryRun    bool     `json:"dry_run"`
	WouldReap []string `json:"would_reap"`
	Reaped    []string `json:"reaped"`
	Skipped   []string `json:"skipped"`
	Blocked   []string `json:"blocked"`
}

// ReapVerifyStacks removes only explicitly owned, terminal, aged stacks.
// Unknown ownership, liveness, timestamps, and project identity are all
// blocked. The default is deliberately dry-run when MaxAge is omitted.
func ReapVerifyStacks(ctx context.Context, client ComposeStackClient, opts ReapVerifyStacksOptions) (ReapVerifyStacksReport, error) {
	if client == nil {
		return ReapVerifyStacksReport{}, fmt.Errorf("compose cleanup: client is required")
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	stacks, err := client.List(ctx)
	if err != nil {
		return ReapVerifyStacksReport{}, fmt.Errorf("compose cleanup: list stacks: %w", err)
	}
	report := ReapVerifyStacksReport{DryRun: opts.DryRun}
	for _, stack := range stacks {
		if stack.Project == "" || stack.WorkingDir == "" || !stack.VerifyHarness {
			if stack.Project != "" && stack.VerifyHarness {
				report.Blocked = append(report.Blocked, stack.Project)
			}
			continue
		}
		if stack.VerifyRunState == VerifyRunLive {
			report.Skipped = append(report.Skipped, stack.Project)
			continue
		}
		if stack.VerifyRunState != VerifyRunFinished && stack.VerifyRunState != VerifyRunFailed {
			report.Blocked = append(report.Blocked, stack.Project)
			continue
		}
		age := opts.Now().Sub(stack.CreatedAt)
		if stack.CreatedAt.IsZero() || age < opts.MaxAge {
			report.Skipped = append(report.Skipped, stack.Project)
			continue
		}
		if opts.DryRun {
			report.WouldReap = append(report.WouldReap, stack.Project)
			continue
		}
		if err := client.Down(ctx, stack.Project, stack.WorkingDir); err != nil {
			return report, fmt.Errorf("compose cleanup: down %s: %w", stack.Project, err)
		}
		report.Reaped = append(report.Reaped, stack.Project)
	}
	sort.Strings(report.WouldReap)
	sort.Strings(report.Reaped)
	sort.Strings(report.Skipped)
	sort.Strings(report.Blocked)
	return report, nil
}
