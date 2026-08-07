package security

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// CLILaunch is the shared entry for cmd/herd pulse/standing/up/review/forge
// spawn paths. It builds policy+grant and calls LaunchAgent — never raw AgentStart.
type CLILaunch struct {
	Spawner       ProcessSpawner
	ControlSecret string
	RepoIdentity  string
	RepoAllowlist []string
	// SharedCheckout is the repository root (denied as agent cwd).
	SharedCheckout string
	// Worktree is the only allowed agent cwd.
	Worktree string
	Role     string
	// PackageAllowlist optional exclusive package roots.
	PackageAllowlist []string
	EventLogPath     string
	// TaskRef bound into session attestation (required).
	TaskRef string
	// LeaseGeneration bound into session attestation (required; never fabricated).
	LeaseGeneration string
	// ClaimLookup proves lease against live FAC-147 claim (required for tasks).
	ClaimLookup LiveClaimLookup
	// SessionResolver required for live agent_session binding.
	SessionResolver LiveAgentResolver
	// TestCommand optional verification profile override.
	TestCommand string
}

// Run starts name/kind in workspace under the sandbox. model is argv only.
func (c CLILaunch) Run(workspace, name, kind, model string) (*AgentSpawnResult, error) {
	if c.Spawner == nil {
		return nil, fmt.Errorf("%w: nil spawner", ErrUnknownPolicy)
	}
	if err := RequireControlSecret(c.ControlSecret); err != nil {
		return nil, err
	}
	role := strings.TrimSpace(c.Role)
	if role == "" {
		return nil, fmt.Errorf("%w: Role required (empty role is fail-closed)", ErrUnknownPolicy)
	}
	repo := c.RepoIdentity
	if repo == "" {
		repo = "herdforge"
	}
	allow := c.RepoAllowlist
	if len(allow) == 0 {
		allow = []string{repo}
	}
	wt := c.Worktree
	if wt == "" {
		return nil, fmt.Errorf("%w: worktree cwd required", ErrPathDenied)
	}
	shared := c.SharedCheckout
	if shared == "" {
		shared = "."
	}
	policy, err := PolicyForLane(role, wt, shared, repo, allow, c.ControlSecret, c.PackageAllowlist)
	if err != nil {
		return nil, err
	}
	if c.TestCommand != "" {
		policy.TestCommand = c.TestCommand
	}
	logPath := c.EventLogPath
	if logPath == "" {
		if shared != "" && shared != "." {
			logPath = filepath.Join(shared, ".herd", "security-events.jsonl")
		} else {
			logPath = filepath.Join(wt, ".herd", "security-events.jsonl")
		}
	}
	if err := BindDurableEvents(policy, logPath, &MemorySink{}); err != nil {
		return nil, err
	}

	taskRef := strings.TrimSpace(c.TaskRef)
	if err := ValidateTaskRef(taskRef); err != nil {
		return nil, err
	}
	lease := strings.TrimSpace(c.LeaseGeneration)
	// Standing leases only when task is explicitly a standing agent session.
	standingOK := strings.HasPrefix(taskRef, "standing") || strings.EqualFold(taskRef, "standing")
	if err := ValidateLeaseGeneration(lease, standingOK); err != nil {
		return nil, err
	}
	if err := ValidateLiveTaskLease(context.Background(), c.ClaimLookup, taskRef, lease, standingOK, "", ""); err != nil {
		return nil, err
	}
	st := StructureTask(taskRef, "", "", role, wt, "", "standing", false)
	tools := []string{"read-file"}
	if strings.EqualFold(role, RoleReviewer) {
		tools = []string{"read-file", "git-read"}
	} else {
		tools = []string{"read-file", "git-write", "write-file"}
	}
	grant, err := policy.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: role, Tools: tools, Structured: st, ProviderText: "",
		Env: map[string]string{"PATH": EnvironMap(nil)["PATH"]},
	})
	if err != nil {
		return nil, err
	}
	return LaunchAgent(c.Spawner, AgentSpawnRequest{
		Policy:          policy,
		Grant:           grant,
		Name:            name,
		Kind:            kind,
		Model:           model,
		Workspace:       workspace,
		Label:           name,
		NoFocus:         true,
		Ambient:         EnvironMap(nil),
		EventLogPath:    logPath,
		TaskRef:         taskRef,
		LeaseGeneration: lease,
		ClaimLookup:     c.ClaimLookup,
		SessionResolver: c.SessionResolver,
	})
}
