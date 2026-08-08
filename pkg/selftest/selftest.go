package selftest

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type AssertionResult struct {
	Name   string
	Passed bool
	Err    error
}

type SelfTestRunner struct {
	RepoRoot string
}

func NewSelfTestRunner(repoRoot string) *SelfTestRunner {
	return &SelfTestRunner{RepoRoot: repoRoot}
}

// RunSuite executes all core behavior assertions and negative pins (porting bin/herd-selftest)
func (s *SelfTestRunner) RunSuite(ctx context.Context) ([]AssertionResult, error) {
	results := []AssertionResult{}

	// 1. Boundary check
	err := preflight.CheckWorktreeBoundary(s.RepoRoot)
	results = append(results, AssertionResult{
		Name:   "preflight_boundary_check",
		Passed: err == nil,
		Err:    err,
	})

	// 2. Provider contract check. FAC-155: even this in-process check goes
	// through the central factory, so the repo holds no production constructor
	// call that bypasses activation policy.
	mp, err := provider.NewProductionProvider(provider.TaskConfig{Type: "memory"})
	if err == nil {
		err = provider.VerifyProviderContract(ctx, mp, "selftest-proj")
	}
	results = append(results, AssertionResult{
		Name:   "provider_contract_check",
		Passed: err == nil,
		Err:    err,
	})

	// 3. Configuration parsing check
	dummyConfig := []byte(`
version: "1"
project:
  name: "selftest-app"
  default_branch: "main"
task_provider:
  type: "memory"
  project_id: "test"
model_providers:
  - name: "claude-test"
    type: "anthropic"
    model: "claude-3-5-sonnet"
roles:
  - name: "test-role"
    provider: "claude-test"
    prompt_path: ".herd/prompts/test.md"
`)
	_, err = config.ParseConfig(dummyConfig)
	results = append(results, AssertionResult{
		Name:   "config_schema_validation",
		Passed: err == nil,
		Err:    err,
	})

	for _, r := range results {
		if !r.Passed {
			return results, fmt.Errorf("selftest assertion failed: %s (%v)", r.Name, r.Err)
		}
	}

	return results, nil
}
