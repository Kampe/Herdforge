package containerlifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// execCommandContext is a package-level seam so tests can fake docker
// invocations, matching pkg/worktree's execCommandContext convention.
var execCommandContext = exec.CommandContext

// DockerRemove force-removes the exact container ID. It takes only an
// ID — never a name, glob, or filter — so a caller cannot widen the
// blast radius by construction. It deliberately does NOT special-case a
// "no such container" style error as success: docker's exact wording
// varies by version/subcommand, and pattern-matching stderr text as the
// signal for "this actually succeeded" is fragile. Whether removal
// actually held is EnsureCleanup's job, via the independent DockerAbsent
// check — not this function's error return.
func DockerRemove(ctx context.Context, containerID string) error {
	if containerID == "" {
		return errors.New("containerlifecycle: empty container id")
	}
	cmd := execCommandContext(ctx, "docker", "rm", "--force", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm %s: %w: %s", containerID, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DockerAbsent reports whether containerID no longer exists. This is the
// authoritative check EnsureCleanup relies on to decide whether a
// container is actually gone, deliberately independent of whatever
// DockerRemove itself reported.
//
// docker inspect has no machine-readable "not found" signal — only a
// non-zero exit and English stderr text, which is the best signal docker
// itself exposes for this. This match is intentionally narrow ("No
// such") and fails CLOSED: any other error (daemon unreachable, timeout,
// permission) is surfaced as an error rather than assumed to mean
// absent, so a transient docker failure can never be misread as proof of
// removal.
func DockerAbsent(ctx context.Context, containerID string) (bool, error) {
	if containerID == "" {
		return false, errors.New("containerlifecycle: empty container id")
	}
	cmd := execCommandContext(ctx, "docker", "inspect", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "No such") {
			return true, nil
		}
		return false, fmt.Errorf("docker inspect %s: %w: %s", containerID, err, strings.TrimSpace(stderr.String()))
	}
	return false, nil
}

// LiveContainer is one row of `docker ps -a` — used only for read-only
// auditing (AuditUnowned), never as a source of cleanup targets.
type LiveContainer struct {
	ID      string
	Image   string
	Status  string
	Names   string
	Created string
}

// DockerListAll lists every container docker currently knows about,
// running or not. It passes --no-trunc: `docker ps` truncates IDs to 12
// characters by default, but receipts are registered under the full ID
// `docker create`/`docker inspect` return, so a truncated ID here would
// never string-match a receipt and every owned container would be
// misreported as unowned.
func DockerListAll(ctx context.Context) ([]LiveContainer, error) {
	cmd := execCommandContext(ctx, "docker", "ps", "-a", "--no-trunc", "--format", "{{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}\t{{.CreatedAt}}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps -a: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, nil
	}
	var containers []LiveContainer
	for _, line := range strings.Split(trimmed, "\n") {
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			continue
		}
		containers = append(containers, LiveContainer{ID: parts[0], Image: parts[1], Status: parts[2], Names: parts[3], Created: parts[4]})
	}
	return containers, nil
}
