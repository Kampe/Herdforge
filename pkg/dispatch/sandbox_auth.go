package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/security"
)

func eventCount(policy *security.LaunchPolicy) int {
	if policy == nil || policy.Events == nil {
		return 0
	}
	switch s := policy.Events.(type) {
	case *security.MemorySink:
		return len(s.Snapshot())
	case *security.MultiSink:
		for _, sub := range s.Sinks {
			if m, ok := sub.(*security.MemorySink); ok {
				return len(m.Snapshot())
			}
		}
	}
	return 0
}

// authorizeAgentSandbox builds and enforces the FAC-133 LaunchPolicy for a
// write-capable agent. Control secret, repo allowlist, FS root, tool allowlist,
// secret denial, and structured provenance all fail closed.
// authorizeAgentSandbox builds and enforces the FAC-133 LaunchPolicy for a
// write-capable agent. Control secret, repo allowlist, FS root, tool allowlist,
// secret denial, and structured provenance all fail closed.
func (d *Dispatcher) authorizeAgentSandbox(lane *config.LaneDef, task *provider.Task, worktreePath string) (*security.LaunchGrant, *security.LaunchPolicy, error) {
	if d == nil {
		return nil, nil, security.ErrUnknownPolicy
	}
	secret := strings.TrimSpace(d.ControlSecret)
	if secret == "" && d.Control != nil {
		secret = strings.TrimSpace(d.Control.Secret)
	}
	if err := security.RequireControlSecret(secret); err != nil {
		return nil, nil, err
	}
	if d.Control == nil || strings.TrimSpace(d.Control.Secret) == "" {
		return nil, nil, fmt.Errorf("%w: control plane required for write-capable launch", security.ErrMissingControlSecret)
	}
	if d.Control.Mailbox == nil {
		return nil, nil, fmt.Errorf("%w: control mailbox required", security.ErrUnknownPolicy)
	}
	// Keep plane secret and dispatcher secret aligned when both are set.
	if d.ControlSecret != "" && d.Control.Secret != d.ControlSecret {
		return nil, nil, fmt.Errorf("%w: control secret mismatch", security.ErrMissingControlSecret)
	}
	if lane == nil {
		return nil, nil, fmt.Errorf("%w: lane required", security.ErrUnknownPolicy)
	}
	role := strings.TrimSpace(lane.Role)
	if role == "" {
		return nil, nil, fmt.Errorf("%w: lane role required (empty role is fail-closed)", security.ErrUnknownPolicy)
	}
	repoID := d.RepoIdentity
	if repoID == "" && d.Config != nil {
		repoID = d.Config.Project.Name
	}
	if repoID == "" {
		repoID = "herdforge"
	}
	allow := d.RepoAllowlist
	if len(allow) == 0 {
		allow = []string{repoID}
	}
	shared := ""
	if d.Worktree != nil {
		shared = d.Worktree.RepoRoot()
	}
	policy, err := security.PolicyForLane(role, worktreePath, shared, repoID, allow, secret, d.PackageAllowlist)
	if err != nil {
		return nil, nil, err
	}
	// Durable + in-memory event sinks (nil drops are forbidden).
	mem := &security.MemorySink{}
	if d.SandboxEvents != nil {
		if m, ok := d.SandboxEvents.(*security.MemorySink); ok {
			mem = m
		}
	}
	// Durable log: tests MUST set SecurityEventLog under t.TempDir(); production
	// uses $repo/.herd/security-events.jsonl (never package test cwd).
	eventLog := d.SecurityEventLog
	if eventLog == "" && shared != "" {
		eventLog = filepath.Join(shared, ".herd", "security-events.jsonl")
	}
	if eventLog == "" {
		return nil, nil, fmt.Errorf("%w: SecurityEventLog required", security.ErrUnknownPolicy)
	}
	if err := security.BindDurableEvents(policy, eventLog, mem); err != nil {
		return nil, nil, err
	}

	st := security.StructureTask(
		task.Ref, task.Title, task.Description,
		role, worktreePath, "", "dispatched", false,
	)
	if err := security.AssertNoProviderControlMutation(st, role, worktreePath); err != nil {
		return nil, policy, err
	}

	providerText := security.ProviderTextBundle(task.Title, task.Description)
	urls := security.ExtractExternalURLs(providerText)
	// Tools the write-capable surface intends to enable (subset of allowlist).
	tools := []string{"read-file"}
	if strings.EqualFold(role, "reviewer") {
		tools = []string{"read-file", "git-read"}
	} else {
		tools = []string{"read-file", "git-write", "write-file"}
	}

	grant, err := policy.AuthorizeLaunch(security.LaunchRequest{
		CWD:          worktreePath,
		Role:         role,
		Tools:        tools,
		Env:          map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")},
		ExternalURLs: urls,
		ProviderText: providerText,
		Structured:   st,
		// Provider text must never request merge/board elevation at launch.
		MergeRequested:      false,
		BoardWriteRequested: false,
	})
	if err != nil {
		return nil, policy, err
	}
	return grant, policy, nil
}
