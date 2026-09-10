package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

// FAC-679: the exact b14 managed run finished every package test and the
// build with PASS, yet the full-suite receipt was BLOCKED because the public
// launch path left an untracked cmd/herd/.herd/toolprobe-cache.json in the
// verification clone. ensureArtifactToolProbe's non-production branch
// persisted its synthetic local-harness receipt through the durable file
// cache, which resolves DefaultCachePath against the process working
// directory. These tests watch the shipped `up` path end to end.

// productionOpenUpRuntime drives runUpCommand through the real production
// launch seam: Open delegates to openWriteCapableTab exactly like
// liveUpRuntime.Open, so the tool-probe branch under repair is on the
// watched public up path. Route/Ready/Start stay faked: no agent is spawned,
// but the tab boundary (probe admission included) is the compiled one.
type productionOpenUpRuntime struct {
	*upFixtureRuntime
	productionOpens int
}

func (r *productionOpenUpRuntime) Open(d *router.LaunchDecision, req launch.Request, l *config.LaneDef, w, n, c string) (*herdr.TabInfo, error) {
	r.productionOpens++
	_, tab, err := openWriteCapableTab(d, req, l, w, n, c)
	return tab, err
}

// candidateDirtyEntries mirrors pkg/verifier.requireCleanCandidate's porcelain
// v2 parse: branch headers are metadata, every other entry is worktree dirt.
func candidateDirtyEntries(t *testing.T, dir string) []string {
	t.Helper()
	out, err := gitCmdForTest(dir, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
	if err != nil {
		t.Fatalf("read candidate status: %v\n%s", err, out)
	}
	var dirty []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		dirty = append(dirty, line)
	}
	return dirty
}

// TestFAC679UpPathKeepsCandidateClean proves the public up path
// (runUpCommand -> production Open seam -> openWriteCapableTab ->
// ensureArtifactToolProbe) leaves no tool-probe cache artifact in the
// candidate working directory. The synthetic local-harness receipt is
// invocation-local evidence and must never become candidate content.
func TestFAC679UpPathKeepsCandidateClean(t *testing.T) {
	root, fixture := upReceiptFixture(t)
	installDummyHarnesses(t)
	// Launch receipts are durable evidence but not candidate content: keep
	// them out of the watched root so the only possible dirt is the probe
	// cache artifact under repair.
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "launch-receipts.jsonl"))
	// A committed baseline makes the cleanliness gate exact: any entry the
	// up path leaves behind is new pollution, not fixture residue.
	if out, err := gitCmdForTest(root, "add", "-A"); err != nil {
		t.Fatalf("stage fixture baseline: %v\n%s", err, out)
	}
	if out, err := gitCmdForTest(root, "commit", "-q", "-m", "fac679 baseline"); err != nil {
		t.Fatalf("commit fixture baseline: %v\n%s", err, out)
	}

	runtime := &productionOpenUpRuntime{upFixtureRuntime: fixture}
	var out bytes.Buffer
	if err := runUpCommand("worker", runtime, &out); err != nil {
		t.Fatalf("public up path failed: %v", err)
	}
	if runtime.productionOpens != 1 || runtime.starts != 1 {
		t.Fatalf("production Open seam not exercised: opens=%d starts=%d", runtime.productionOpens, runtime.starts)
	}
	if !strings.Contains(out.String(), "started:") {
		t.Fatalf("up did not report success: %s", out.String())
	}
	if dirty := candidateDirtyEntries(t, root); len(dirty) > 0 {
		t.Fatalf("public up path polluted the candidate (FAC-679):\n%s", strings.Join(dirty, "\n"))
	}
	if _, err := os.Stat(filepath.Join(root, toolprobe.DefaultCachePath)); !os.IsNotExist(err) {
		t.Fatalf("tool-probe cache artifact left in candidate: %v", err)
	}
}

// TestFAC679StrictProbeStillWritesDurableCache is the production
// durable-cache control: under the strict tool-probe policy the same seam
// must keep using the durable file cache and the live artifact probe. The
// repair may not pass by disabling durable persistence or admission.
func TestFAC679StrictProbeStillWritesDurableCache(t *testing.T) {
	// The durable write is deliberate, so it must land in an isolated cwd —
	// never in this package's source tree.
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "1")
	t.Setenv("HERD_LOCAL_TOOL_PROBE", "strict")
	// Probe-faithful harness stubs go FIRST on PATH so the live artifact
	// probe executes a stub that really writes the sentinel; system dirs
	// stay reachable for the stub's own mkdir/dirname.
	t.Setenv("PATH", stubHarnessPATH(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	d, err := testLaunchRouter(t).Decide(launchBoundaryDecideRequest())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ensureArtifactToolProbe(context.Background(), d)
	if err != nil {
		t.Fatalf("strict tool-probe must pass against a probe-faithful harness: %v", err)
	}
	if !receipt.Passes(time.Now().UTC()) {
		t.Fatalf("strict tool-probe returned non-PASS receipt: %+v", receipt)
	}
	cachePath := filepath.Join(root, toolprobe.DefaultCachePath)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("strict tool-probe must still write the durable cache: %v", err)
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), id.Key()) {
		t.Fatalf("durable cache does not carry the probed identity %s", id.Key())
	}
}

// TestFAC679CleanGateDetectsToolprobeCacheArtifact is the dirty-candidate
// control: the cleanliness gate used above must actually flag the exact
// artifact the b14 managed run left behind, so the regression cannot pass by
// weakening candidate validation.
func TestFAC679CleanGateDetectsToolprobeCacheArtifact(t *testing.T) {
	root := t.TempDir()
	if out, err := gitCmdForTest(root, "init", "-q", "-b", "wt/fac679"); err != nil {
		t.Fatalf("fixture init: %v\n%s", err, out)
	}
	if out, err := gitCmdForTest(root, "commit", "-q", "--allow-empty", "-m", "base"); err != nil {
		t.Fatalf("fixture base: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, toolprobe.DefaultCachePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, toolprobe.DefaultCachePath), []byte(`{"version":1,"items":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty := candidateDirtyEntries(t, root)
	found := false
	for _, entry := range dirty {
		if strings.Contains(entry, toolprobe.DefaultCachePath) {
			found = true
		}
	}
	if !found {
		t.Fatalf("clean gate did not flag the tool-probe cache artifact: %v", dirty)
	}
}
