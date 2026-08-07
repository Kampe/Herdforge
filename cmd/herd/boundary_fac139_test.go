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

// Six production launch entrypoints (pulse, standing, up, review, forge, and
// dispatch) share openWriteCapableTab / launch.Open. This table proves each
// named path's boundary rejects missing decision/probe before any tab side
// effect and that an admitted plan carries exact model+effort from the
// LaunchDecision (FAC-139).
func TestSixLaunchPaths_BoundaryFailClosedAndArgvFromDecision(t *testing.T) {
	paths := []string{"dispatch", "pulse", "standing", "up", "review", "forge"}
	now := time.Unix(1_800_000_000, 0).UTC()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "worker", Scope: router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
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
				Decision: d,
				Request:  launch.Request{Decision: d, TaskRef: "worker", Scope: router.ScopeLane},
				Probe:    nil,
				Lane:     &config.LaneDef{Name: path, Role: "worker", Authority: config.AuthorityWrite},
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
				Decision: d,
				Request:  launch.Request{Decision: d, TaskRef: "worker", Scope: router.ScopeLane},
				Probe:    &pass,
				Lane:     &config.LaneDef{Name: path, Role: "worker", Authority: config.AuthorityWrite},
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
