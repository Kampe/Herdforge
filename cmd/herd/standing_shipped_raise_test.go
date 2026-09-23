package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/procsignal"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// standingShippedRaiseLab is a disposable git identity + fake-herdr raise
// fixture. Every file is written under root; the stamped CLI is invoked with
// Dir=root so process cwd cannot leak prompts into the package checkout.
func standingShippedRaiseLab(t *testing.T) (root, origin, laneDir string) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	root = t.TempDir()
	origin = filepath.Join(root, "origin")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	standingGit(t, origin, "init", "-q", "-b", "main")
	standingGit(t, origin, "config", "user.email", "t@t")
	standingGit(t, origin, "config", "user.name", "t")
	_ = standingCommit(t, origin, "base")
	laneDir = filepath.Join(root, "wt", "scout")
	if err := os.MkdirAll(filepath.Dir(laneDir), 0o755); err != nil {
		t.Fatal(err)
	}
	standingGit(t, root, "clone", "-q", origin, laneDir)

	standingGit(t, root, "init", "-q", "-b", "main")
	standingGit(t, root, "commit", "-q", "--allow-empty", "-m", "identity")
	standingGit(t, root, "remote", "add", "origin", "https://github.com/Kampe/Herdforge.git")
	instead := "url." + origin + "/.insteadOf"
	standingGit(t, root, "config", instead, "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "remote", "set-url", "origin", "https://github.com/Kampe/Herdforge.git")
	standingGit(t, laneDir, "config", instead, "https://github.com/Kampe/Herdforge.git")

	promptAbs := filepath.Join(root, ".herd", "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptAbs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptAbs, []byte("prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `version: "1"
project:
  name: fac621-lab
task_provider:
  type: kaneo
  project_id: lab
fleet:
  herdr_workspace: wFAKE
lanes:
  - name: scout
    role: scout-planner
    agent_kind: grok
    harness: grok
    prompt: .herd/prompts/worker.md
    worktree: wt/scout
    provider: grok
    model: grok-4.6
    effort: medium
    task_shape: architecture
    standing: true
    authority: write
    capabilities: ["git-write"]
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	winddown := filepath.Join(root, ".herd", "winddown.json")
	if err := os.WriteFile(winddown, []byte(`{"enabled":false,"actor":"test","reason":"fac-621","timestamp":"2026-09-22T00:00:00Z","generation":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, origin, laneDir
}

func standingShippedRaiseQuota(t *testing.T, home, cachePath string) {
	t.Helper()
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"lab":{"key":"fixture-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("herd-account-v1:grok:lab"))
	key := hex.EncodeToString(sum[:])[:10]
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"grok": map[string]any{
				"observed_at": now,
				"account_key": key,
				"provider": usage.ProviderUsage{
					DisplayName: "grok",
					Account:     &usage.AccountIdentity{Key: key, Provenance: "grok-auth:auth.json:entry-key"},
					ObservedAt:  now,
					Resources: map[string]usage.ResourceUsage{
						"weekly": {Kind: "consumption", State: "active", Unit: "percent", Limit: 100, Used: 10, Remaining: 90, Utilization: 0.1, WindowSeconds: 604800},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStandingShippedRaiseCLIRefreshesBehindWorktree(t *testing.T) {
	root, origin, laneDir := standingShippedRaiseLab(t)
	fresh := standingCommit(t, origin, "origin-ahead")

	fakeBin, _ := installProtocolFakeHerdr(t)
	home := t.TempDir()
	quotaCache := filepath.Join(t.TempDir(), "quota.json")
	standingShippedRaiseQuota(t, home, quotaCache)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte("#!/bin/sh\nprintf 'PROBE_OK'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	routeDir := t.TempDir()

	binary := buildHerd(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := procsignal.CommandContext(ctx, binary, "standing", "--only", "scout")
	cmd.Dir = root
	cmd.Env = append(reviewTestEnv(),
		"HOME="+home,
		"HERD_ROOT="+root,
		"HERD_REPO_ROOT="+root,
		"HERD_PROJECT_ROOT="+root,
		"HERD_CONFIG_PATH="+filepath.Join(root, ".herd", "herd.yaml"),
		"HERD_WINDDOWN_STATE="+filepath.Join(root, ".herd", "winddown.json"),
		"HERD_QUOTA_CACHE_PATH="+quotaCache,
		"HERDR_ROUTE_STATE_DIR="+routeDir,
		"HERD_ERA_PROVIDERS=grok",
		"HERD_MODE=local",
		"HERD_WORKSPACE=wFAKE",
		"HERDR_WORKSPACE_ID=wFAKE",
		"HERD_HERDR_BIN="+fakeBin,
		"HERD_NO_LIVE_HERDR=1",
		"HERD_FAKE_LOG="+os.Getenv("HERD_FAKE_LOG"),
		"HERD_FAKE_STATE="+os.Getenv("HERD_FAKE_STATE"),
		"PATH="+binDir+string(os.PathListSeparator)+filepath.Dir(fakeBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	applyNestedSlotReentry(cmd)

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("stamped herd standing hung %s: %s", elapsed, out)
	}
	if err != nil {
		t.Fatalf("stamped herd standing failed after %s: %v\n%s", elapsed, err, out)
	}

	got := standingGit(t, laneDir, "rev-parse", "HEAD")
	if got != fresh {
		t.Fatalf("PrepareWorktree did not refresh: HEAD %s want %s\n%s", got, fresh, out)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, ".herd", "standing-admitted", "scout.json"))
	if readErr != nil {
		t.Fatalf("admitted base missing after shipped raise: %v\n%s", readErr, out)
	}
	var rec standingAdmittedBase
	if json.Unmarshal(raw, &rec) != nil || rec.BaseSHA != fresh {
		t.Fatalf("admitted record %+v want %s\n%s", rec, fresh, out)
	}
	if _, statErr := os.Stat(filepath.Join(".", ".herd", "prompts", "worker.md")); statErr == nil {
		t.Fatal("prompt leaked into process cwd")
	}
}
