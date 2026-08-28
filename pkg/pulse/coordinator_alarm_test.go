package pulse

import (
	"strings"
	"testing"
	"time"
)

// FAC-633: a paused coordinator cannot perform its own recovery beat. Observe
// mode must therefore be a loud failure for that one condition, while normal
// would-run actions remain advisory.
func TestPausedCoordinatorObserveFailsLoudlyButActCanRecover(t *testing.T) {
	obs := Observation{
		Provider: ProviderObservation{Known: true},
		Herdr: HerdrObservation{Known: true, Agents: []AgentObservation{{
			Name: "forge-orchestrator", Status: StatusPaused, Coordinator: true,
		}}},
		Quota:    QuotaObservation{Known: true},
		Review:   ReviewObservation{Known: true},
		WindDown: WindDownObservation{Known: true},
	}
	now := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)

	observe, err := Plan(obs, Options{Now: now, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if observe.ExitCode == 0 {
		t.Fatal("paused coordinator in observe mode must exit non-zero; an advisory-only beat cannot wake itself")
	}
	if got := observe.PausedCoordinators; len(got) != 1 || got[0] != "forge-orchestrator" {
		t.Fatalf("paused coordinators = %v, want the configured coordinator identity", got)
	}
	if out := FormatHuman(observe); !strings.Contains(out, "CONTROL-PLANE STALL: paused coordinator(s): forge-orchestrator") {
		t.Fatalf("paused coordinator alarm is not loud or actionable:\n%s", out)
	}

	act, err := Plan(obs, Options{Act: true, Now: now, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if act.ExitCode != 0 {
		t.Fatalf("act-mode paused coordinator should let the external daemon attempt recovery, exit=%d", act.ExitCode)
	}
}

func TestOrdinaryObserveWouldRunRemainsAdvisory(t *testing.T) {
	obs := Observation{
		Provider: ProviderObservation{Known: true, Claimable: 1},
		Herdr:    HerdrObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		Review:   ReviewObservation{Known: true},
		WindDown: WindDownObservation{Known: true},
	}
	snap, err := Plan(obs, Options{Now: time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.WouldRun == 0 {
		t.Fatal("fixture did not produce an ordinary would-run action")
	}
	if snap.ExitCode != 0 {
		t.Fatalf("ordinary observe-mode would-run must remain advisory, exit=%d", snap.ExitCode)
	}
}
