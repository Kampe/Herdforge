package herdr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Native review verification budget. Concurrent go test ./cmd/herd plus
// make test-unit on a live FAC-607 AGY review drove CPU admission to REFUSE.
// Launch inherits these values through herdr tab create --env so reviewer
// children cannot raise Go parallelism by accident.
const (
	ReviewBudgetGOMAXPROCS = "2"
	ReviewBudgetGOFLAGS    = "-p=1"
)

// ReviewLaunchEnv is the KEY=VALUE set every native review tab must inherit.
// AgentRoleEnv stays first; budget pins follow unless the caller already set
// the same keys.
func ReviewLaunchEnv() []string {
	return withReviewVerificationBudget([]string{AgentRoleEnv})
}

func ReviewVerificationBudgetEnv() []string {
	return []string{
		"GOMAXPROCS=" + ReviewBudgetGOMAXPROCS,
		"GOFLAGS=" + ReviewBudgetGOFLAGS,
	}
}

func withReviewVerificationBudget(env []string) []string {
	out := append([]string(nil), env...)
	for _, pin := range ReviewVerificationBudgetEnv() {
		key, _, _ := strings.Cut(pin, "=")
		explicit := false
		for _, entry := range env {
			if k, _, ok := strings.Cut(entry, "="); ok && k == key {
				explicit = true
				break
			}
		}
		if !explicit {
			out = append(out, pin)
		}
	}
	return out
}

// ReviewTabCreate starts a native review tab with the verification budget
// inherited through herdr tab create --env. Callers still pass an absolute
// candidate cwd; empty workspace or cwd fail closed.
func ReviewTabCreate(workspace, label, cwd string) (*TabInfo, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("herdr tab create: workspace is required (no hardcoded fallback)")
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("herdr tab create: cwd is required for review agents")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: resolve cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("herdr tab create: cwd %q is not a directory", abs)
	}
	return TabCreate(TabCreateOptions{
		Workspace: workspace,
		Label:     label,
		Cwd:       abs,
		NoFocus:   true,
		Env:       bindChildWorkspaceEnv(abs, workspace, ReviewLaunchEnv()),
	})
}
