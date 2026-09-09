package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/router"
)

// twoWorkerLanes is the live FAC-608 shape: role "worker" is NOT unique, so a
// role is not an identity. smith is first in file order; smith-grok is the lane
// an operator names explicitly with `herd dispatch --lane smith-grok`.
func twoWorkerLanes() *config.Config {
	return &config.Config{Lanes: []config.LaneDef{
		{
			Name: "smith", Role: "worker",
			AgentKind: "codex", Harness: "codex",
			Provider: testWorkerProvider, Model: testWorkerModel, Effort: testWorkerEffort,
			TaskShape: "implementation",
		},
		{
			Name: "smith-grok", Role: "worker",
			AgentKind: "grok", Harness: "grok",
			Provider: "grok", Model: "grok-4.6", Effort: "medium",
			TaskShape: "implementation",
		},
	}}
}

// FAC-703 mutation check: dispatchTicketDecision must hand launch admission
// findLaneByName(cfg, canonicalLane.Name). Substituting canonicalLane.Role
// (or findLaneForRole) is the live 2026-09-01 defect.
func TestDispatchTicketDecisionBindsLaunchLaneByNameNotRole(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractFuncBody(string(src), "func dispatchTicketDecision(")
	if fn == "" {
		t.Fatal("dispatchTicketDecision not found")
	}
	if !strings.Contains(fn, "findLaneByName(cfg, canonicalLane.Name)") {
		t.Fatal("dispatchTicketDecision must bind launch admission to canonicalLane.Name")
	}
	if strings.Contains(fn, "findLaneByName(cfg, canonicalLane.Role)") {
		t.Fatal("dispatchTicketDecision re-resolved the explicit lane by role name; that is the FAC-608 collapse")
	}
	if strings.Contains(fn, "findLaneForRole(cfg, canonicalLane.Role)") || strings.Contains(fn, "findLaneForRole(cfg, canonicalLane.Name)") {
		t.Fatal("dispatchTicketDecision must not re-resolve the explicit lane through findLaneForRole")
	}
}

func extractFuncBody(src, sig string) string {
	idx := strings.Index(src, sig)
	if idx < 0 {
		return ""
	}
	rest := src[idx:]
	brace := strings.Index(rest, "{")
	if brace < 0 {
		return ""
	}
	depth := 0
	for i := brace; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	return rest
}

// FAC-703: reproduction of the 2026-09-01 live defect. dispatchTicketDecision
// resolved canonicalLane by NAME, then handed launch admission only
// canonicalLane.Role. Admission called findLaneForRole, which returns the FIRST
// lane with that role -- so `--lane smith-grok` passed admission and launched
// smith (codex/gpt-5.6-luna). The operator's explicit lane choice was silently
// replaced by file order.
//
// The lane that reaches the route callback IS the lane that will be launched,
// so asserting on it asserts on the argv.
func TestExplicitLaneSurvivesLaunchAdmission(t *testing.T) {
	cfg := twoWorkerLanes()
	want := &cfg.Lanes[1] // smith-grok, named explicitly by the operator

	var routed *config.LaneDef
	_, err := launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, want, true,
		func(lane *config.LaneDef) (*router.LaunchDecision, error) {
			routed = lane
			return nil, errStopBeforeSideEffect
		},
		func(*router.LaunchDecision) error {
			t.Fatal("effect ran after the route refused")
			return nil
		})
	if err == nil {
		t.Fatal("route error was swallowed")
	}
	if routed == nil {
		t.Fatal("admission never reached the route")
	}
	if routed.Name != "smith-grok" {
		t.Fatalf("admission re-resolved the explicit lane by role: routed %q, want %q "+
			"(this is the FAC-608 collapse: first lane of the shared role wins)", routed.Name, want.Name)
	}
	if routed.Provider != "grok" || routed.Model != "grok-4.6" {
		t.Fatalf("routed lane carries the wrong launch tuple: %s/%s; want grok/grok-4.6",
			routed.Provider, routed.Model)
	}
}

// The two lanes must stay distinguishable in BOTH directions; a fix that simply
// returns the last worker lane instead of the first is the same bug mirrored.
func TestEachSharedRoleLaneAdmitsAsItself(t *testing.T) {
	cfg := twoWorkerLanes()
	for i := range cfg.Lanes {
		lane := &cfg.Lanes[i]
		t.Run(lane.Name, func(t *testing.T) {
			var routed *config.LaneDef
			_, _ = launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, lane, true,
				func(got *config.LaneDef) (*router.LaunchDecision, error) {
					routed = got
					return nil, errStopBeforeSideEffect
				},
				func(*router.LaunchDecision) error { return nil })
			if routed == nil || routed.Name != lane.Name {
				t.Fatalf("lane %q did not admit as itself: routed %v", lane.Name, routed)
			}
		})
	}
}

// A lane that is not part of the compiled config must never be launched just
// because its role matches one that is. Admission is an identity fence, not a
// role lookup.
func TestUnconfiguredLaneIsRefused(t *testing.T) {
	cfg := twoWorkerLanes()
	imposter := &config.LaneDef{
		Name: "smith-imposter", Role: "worker",
		AgentKind: "codex", Harness: "codex",
		Provider: testWorkerProvider, Model: testWorkerModel, Effort: testWorkerEffort,
		TaskShape: "implementation",
	}
	routed := false
	_, err := launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, imposter, true,
		func(*config.LaneDef) (*router.LaunchDecision, error) {
			routed = true
			return nil, errStopBeforeSideEffect
		},
		func(*router.LaunchDecision) error { return nil })
	if err == nil || routed {
		t.Fatalf("an unconfigured lane was admitted: routed=%v err=%v", routed, err)
	}
}

// The live defect was substituting canonicalLane.Role for canonicalLane.Name
// after the registry had already resolved smith-grok by name. Role lookup
// returns the FIRST worker lane (smith / codex). This test is the mutation
// fixture: if production ever does that again, TestExplicitLaneSurvivesLaunchAdmission
// is the RED counterpart, and this test documents why.
func TestRoleSubstitutionCollapsesExplicitLaneToFirstWorker(t *testing.T) {
	cfg := twoWorkerLanes()
	registry, err := canonicalLaneRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	canonicalLane, err := registry.ResolveLaneName("smith-grok")
	if err != nil {
		t.Fatal(err)
	}
	if canonicalLane != (lifecycle.CanonicalLane{Name: "smith-grok", Role: "worker"}) {
		t.Fatalf("registry resolved %+v, want smith-grok/worker", canonicalLane)
	}
	byName := findLaneByName(cfg, canonicalLane.Name)
	byRole := findLaneForRole(cfg, canonicalLane.Role)
	if byName == nil || byName.Name != "smith-grok" || byName.Provider != "grok" || byName.Model != "grok-4.6" {
		t.Fatalf("name lookup lost the explicit lane: %+v", byName)
	}
	if byRole == nil || byRole.Name != "smith" || byRole.Provider != "codex" {
		t.Fatalf("role mutation fixture broken: findLaneForRole(%q) = %+v, want smith/codex (the FAC-608 collapse)", canonicalLane.Role, byRole)
	}
}

// errStopBeforeSideEffect halts admission at the route so these tests assert on
// lane selection alone, with no live router, probe, or quota dependency.
var errStopBeforeSideEffect = errors.New("stop: lane selection asserted")
