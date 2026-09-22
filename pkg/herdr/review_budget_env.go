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
// AgentRoleEnv stays first. GOMAXPROCS is pinned to the budget. Existing
// GOFLAGS (caller slice, else the launcher process environment) are kept and
// only the bounded -p setting is forced to -p=1.
func ReviewLaunchEnv() []string {
	env := []string{AgentRoleEnv}
	if g := strings.TrimSpace(os.Getenv("GOFLAGS")); g != "" {
		env = append(env, "GOFLAGS="+g)
	}
	return withReviewVerificationBudget(env)
}

func ReviewVerificationBudgetEnv() []string {
	return []string{
		"GOMAXPROCS=" + ReviewBudgetGOMAXPROCS,
		"GOFLAGS=" + ReviewBudgetGOFLAGS,
	}
}

func mergeBoundedGOFLAGS(existing string) string {
	fields := strings.Fields(strings.TrimSpace(existing))
	out := make([]string, 0, len(fields)+1)
	skipValue := false
	for i := 0; i < len(fields); i++ {
		if skipValue {
			skipValue = false
			continue
		}
		f := fields[i]
		if f == "-p" {
			if i+1 < len(fields) {
				skipValue = true
			}
			continue
		}
		if strings.HasPrefix(f, "-p=") {
			continue
		}
		out = append(out, f)
	}
	out = append(out, ReviewBudgetGOFLAGS)
	return strings.Join(out, " ")
}

func withReviewVerificationBudget(env []string) []string {
	out := make([]string, 0, len(env)+2)
	sawFLAGS := false
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		switch key {
		case "GOMAXPROCS":
			continue
		case "GOFLAGS":
			out = append(out, "GOFLAGS="+mergeBoundedGOFLAGS(val))
			sawFLAGS = true
		default:
			out = append(out, entry)
		}
	}
	out = append(out, "GOMAXPROCS="+ReviewBudgetGOMAXPROCS)
	if !sawFLAGS {
		out = append(out, "GOFLAGS="+ReviewBudgetGOFLAGS)
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
