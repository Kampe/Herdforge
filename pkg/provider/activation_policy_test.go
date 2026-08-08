package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

// TestActivationPolicy_EnabledGate pins FAC-155's activation authority: a
// provider type activates only when the repository explicitly enables it.
//
// Non-vacuous by construction — the "disabled"/"mismatch" rows fail the moment
// checkEnabled stops gating, and the "allowed" rows fail if the gate over-
// rejects, so neither deleting the gate nor hardcoding `return nil` passes.
func TestActivationPolicy_EnabledGate(t *testing.T) {
	cases := []struct {
		name       string
		tc         TaskConfig
		wantErr    bool
		wantErrHas string
		wantInner  string
	}{
		{
			name:      "declared type with no policy activates itself",
			tc:        TaskConfig{Type: "memory"},
			wantInner: "*provider.MemoryProvider",
		},
		{
			name:      "type listed in enabled activates",
			tc:        TaskConfig{Type: "kaneo", Enabled: []string{"kaneo"}},
			wantInner: "*provider.KaneoProvider",
		},
		{
			name:      "enabled matching is case/space insensitive",
			tc:        TaskConfig{Type: "  KANEO ", Enabled: []string{" Kaneo "}},
			wantInner: "*provider.KaneoProvider",
		},
		{
			name:       "dormant adapter is refused even though it is compiled in",
			tc:         TaskConfig{Type: "jira", Enabled: []string{"kaneo"}},
			wantErr:    true,
			wantErrHas: "not in task_provider.enabled",
		},
		{
			name:       "drifted type against the operator policy fails closed",
			tc:         TaskConfig{Type: "memory", Enabled: []string{"linear"}},
			wantErr:    true,
			wantErrHas: "not in task_provider.enabled",
		},
		{
			name:       "empty type is a hard error, never a default board",
			tc:         TaskConfig{Type: "   "},
			wantErr:    true,
			wantErrHas: "task_provider.type is required",
		},
		{
			name:       "unknown type is a hard error, never MemoryProvider",
			tc:         TaskConfig{Type: "trello"},
			wantErr:    true,
			wantErrHas: "not activated in this build",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp, err := NewProductionProvider(c.tc)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got provider %T", tp)
				}
				if !strings.Contains(err.Error(), c.wantErrHas) {
					t.Fatalf("error %q must contain %q", err, c.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewProductionProvider: %v", err)
			}
			bound, ok := tp.(*BoundClient)
			if !ok {
				t.Fatalf("provider=%T, want *BoundClient", tp)
			}
			if got := typeName(bound.Inner); got != c.wantInner {
				t.Fatalf("inner=%s, want %s", got, c.wantInner)
			}
		})
	}
}

// TestActivationPolicy_DisabledNeverResolvesCredentials proves the config path
// rejects a disabled provider before it touches credential material, so a
// drifted type cannot leak an operator credential into an activation attempt.
func TestActivationPolicy_DisabledNeverResolvesCredentials(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "linear-real-key")
	cfg := &config.Config{TaskProvider: config.TaskProvider{
		Type:      "linear",
		ProjectID: "p",
		APIKeyEnv: "LINEAR_API_KEY",
		Enabled:   []string{"kaneo"},
	}}
	tp, err := NewFromHerdConfig(cfg)
	if err == nil {
		t.Fatalf("disabled linear must not activate, got %T", tp)
	}
	if !strings.Contains(err.Error(), "not in task_provider.enabled") {
		t.Fatalf("error %q must name the enabled-policy violation", err)
	}

	// And the same config with linear enabled must still work, or the test
	// above would pass for the wrong reason (e.g. a blanket refusal).
	cfg.TaskProvider.Enabled = []string{"linear"}
	if _, err := NewFromHerdConfig(cfg); err != nil {
		t.Fatalf("enabled linear must activate: %v", err)
	}
}

// TestActivationPolicy_LiveRepoConfigActivatesExactlyOneAdapter loads this
// repository's real .herd/herd.yaml and proves the configured board is the one
// that activates — and that every other adapter is refused under that same
// config. This is the drift alarm: if someone edits task_provider.type without
// moving task_provider.enabled, this fails.
func TestActivationPolicy_LiveRepoConfigActivatesExactlyOneAdapter(t *testing.T) {
	root := repoRoot(t)
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		t.Fatalf("load repo config: %v", err)
	}
	if len(cfg.TaskProvider.Enabled) != 1 {
		t.Fatalf("repo must enable exactly one provider, got %v", cfg.TaskProvider.Enabled)
	}
	configured := strings.ToLower(strings.TrimSpace(cfg.TaskProvider.Type))
	if strings.ToLower(strings.TrimSpace(cfg.TaskProvider.Enabled[0])) != configured {
		t.Fatalf("task_provider.type %q is not the enabled provider %v", configured, cfg.TaskProvider.Enabled)
	}

	// Every other adapter this build compiles must be refused under the live
	// policy — dormant, not merely unused.
	for _, dormant := range []string{"kaneo", "linear", "jira", "azure", "github", "memory"} {
		if dormant == configured {
			continue
		}
		drifted := *cfg
		drifted.TaskProvider.Type = dormant
		if tp, err := NewFromHerdConfig(&drifted); err == nil {
			t.Fatalf("dormant provider %q activated under the live policy: %T", dormant, tp)
		}
	}
}

// TestActivationPolicy_NoProductionConstructorBypassesFactory scans every
// non-test Go file outside pkg/provider for a direct adapter constructor call.
// This is what makes "reachable only through the central factory" a checked
// property instead of a claim: adding `provider.NewMemoryProvider()` back into
// a command fails this test.
func TestActivationPolicy_NoProductionConstructorBypassesFactory(t *testing.T) {
	root := repoRoot(t)
	banned := []string{
		"NewMemoryProvider(",
		"NewKaneoProvider(",
		"NewLinearProvider(",
		"NewGitHubProvider(",
		"NewJiraProvider(",
		"NewAzureDevOpsProvider(",
	}
	providerPkg := filepath.Join(root, "pkg", "provider")

	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || strings.HasPrefix(info.Name(), ".herd") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(path, providerPkg+string(filepath.Separator)) {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Guard against the scan silently covering nothing (a vacuous pass).
	if scanned < 50 {
		t.Fatalf("scanned only %d production files; the walk is not covering the repo", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("production code must construct providers via provider.NewProductionProvider / NewFromHerdConfig only:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *MemoryProvider:
		return "*provider.MemoryProvider"
	case *KaneoProvider:
		return "*provider.KaneoProvider"
	case *LinearProvider:
		return "*provider.LinearProvider"
	case *GitHubProvider:
		return "*provider.GitHubProvider"
	case *JiraProvider:
		return "*provider.JiraProvider"
	case *AzureDevOpsProvider:
		return "*provider.AzureDevOpsProvider"
	}
	return "unknown"
}

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate module root")
	return ""
}
