package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

// isolateLaunchBoundaryRoutingEnv owns HERD_MODE / HERD_USE_PI so inherited
// fleet metadata cannot select native Codex routing against this Pi-only
// fixture (FAC-711). PATH is a temp dir with no Codex and no live herd-route.
func isolateLaunchBoundaryRoutingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "1")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
}

func launchBoundaryDecideRequest() router.LaunchRequest {
	return router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "worker", Scope: router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	}
}

// Removing isolateLaunchBoundaryRoutingEnv from the table test turns this RED
// under the exact FAC-709 self-gate env: HERD_MODE=local HERD_USE_PI=0 and no
// Codex CLI on PATH (CLI missing at Decide, boundary_fac139_test.go:29).
func TestSixLaunchPaths_InheritedFleetMetadataFailsWithoutIsolation(t *testing.T) {
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("PATH", t.TempDir())
	_, err := testLaunchRouter(t).Decide(launchBoundaryDecideRequest())
	if err == nil {
		t.Fatal("inherited HERD_USE_PI=0 must fail the Pi-only fixture when Codex CLI is absent")
	}
	if !strings.Contains(err.Error(), "CLI missing") {
		t.Fatalf("want CLI missing under inherited native routing, got %v", err)
	}
}

// Six production launch entrypoints (pulse, standing, up, review, forge, and
// dispatch) share openWriteCapableTab / launch.Open. This table proves each
// named path's boundary rejects missing decision/probe before any tab side
// effect and that an admitted plan carries exact model+effort from the
// LaunchDecision (FAC-139).
func TestSixLaunchPaths_BoundaryFailClosedAndArgvFromDecision(t *testing.T) {
	isolateLaunchBoundaryRoutingEnv(t)
	paths := []string{"dispatch", "pulse", "standing", "up", "review", "forge"}
	now := time.Unix(1_800_000_000, 0).UTC()
	d, err := testLaunchRouter(t).Decide(launchBoundaryDecideRequest())
	if err != nil {
		t.Fatal(err)
	}
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:ok", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		t.Run(path+"/missing-probe", func(t *testing.T) {
			opened := 0
			_, _, _, err := launch.Open(tabCountOpener{n: &opened}, launch.BoundarySpec{
				Decision:  d,
				Request:   launch.Request{Decision: d, TaskRef: "worker", Scope: router.ScopeLane},
				Probe:     nil,
				Lane:      &config.LaneDef{Name: path, Role: "worker", Authority: config.AuthorityWrite},
				Workspace: "w1", Label: "forge-" + path, Cwd: t.TempDir(), NoFocus: true,
				Now: func() time.Time { return now },
			})
			if err == nil || opened != 0 {
				t.Fatalf("%s: missing probe opened=%d err=%v", path, opened, err)
			}
		})
		t.Run(path+"/admitted-argv", func(t *testing.T) {
			opened := 0
			var gotEnv []string
			plan, tabID, paneID, err := launch.Open(tabCountOpener{n: &opened, envOut: &gotEnv}, launch.BoundarySpec{
				Decision:  d,
				Request:   launch.Request{Decision: d, TaskRef: "worker", Scope: router.ScopeLane},
				Probe:     &pass,
				Lane:      &config.LaneDef{Name: path, Role: "worker", Authority: config.AuthorityWrite},
				Workspace: "w1", Label: "forge-" + path, Cwd: t.TempDir(),
				Env: []string{"HERD_PATH=" + path}, NoFocus: true,
				Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if opened != 1 || tabID == "" || paneID == "" {
				t.Fatalf("tab open: opened=%d tab=%s pane=%s", opened, tabID, paneID)
			}
			if plan.Model != d.Model || plan.Effort != d.Effort {
				t.Fatalf("plan must use decision model/effort: got %s/%s want %s/%s",
					plan.Model, plan.Effort, d.Model, d.Effort)
			}
			if strings.Join(plan.Argv, "\x00") != strings.Join(d.Argv, "\x00") {
				t.Fatalf("%s: argv not from LaunchDecision", path)
			}
			if len(gotEnv) != 1 || gotEnv[0] != "HERD_PATH="+path {
				t.Fatalf("env not forwarded: %v", gotEnv)
			}
			// Negative mutation: empty model on plan would mean harness default.
			if plan.Model == "" || plan.Effort == "" {
				t.Fatal("plan must not inherit empty harness defaults")
			}
		})
	}
}

type tabCountOpener struct {
	n      *int
	envOut *[]string
}

func (o tabCountOpener) OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
	*o.n++
	if o.envOut != nil {
		*o.envOut = append([]string(nil), env...)
	}
	return "tab-" + label, "pane-" + label, nil
}
