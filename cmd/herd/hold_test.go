package main

import (
	"context"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

type countingHoldBoundary struct {
	generationReads int
	holds           int
	releases        int
}

func (b *countingHoldBoundary) Close() error { return nil }
func (b *countingHoldBoundary) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	b.generationReads++
	return 1, nil
}
func (b *countingHoldBoundary) HasCurrent(context.Context, lifecycle.HoldIdentity) (bool, error) {
	return false, nil
}
func (b *countingHoldBoundary) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{}, nil
}
func (b *countingHoldBoundary) Hold(context.Context, lifecycle.HoldIdentity, string, string, string, int64, *time.Time) (lifecycle.HoldRecord, error) {
	b.holds++
	return lifecycle.HoldRecord{}, nil
}
func (b *countingHoldBoundary) Release(context.Context, lifecycle.HoldIdentity, string, string, string, int64) (lifecycle.HoldRecord, error) {
	b.releases++
	return lifecycle.HoldRecord{}, nil
}

func TestParseHoldExpirySupportsAbsoluteAndBoundedDuration(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	abs, err := parseHoldExpiry("2026-08-04T13:00:00Z", func() time.Time { return now })
	if err != nil || abs == nil || !abs.Equal(now.Add(time.Hour)) {
		t.Fatalf("absolute expiry=%v err=%v", abs, err)
	}
	dur, err := parseHoldExpiry("2h", func() time.Time { return now })
	if err != nil || dur == nil || !dur.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("duration expiry=%v err=%v", dur, err)
	}
	if _, err := parseHoldExpiry("168h1m", func() time.Time { return now }); err == nil {
		t.Fatal("unbounded duration accepted")
	}
}

func TestCanonicalHoldIdentityUsesConfiguredLaneAndFailsUnknownRole(t *testing.T) {
	registry, err := canonicalLaneRegistry(&config.Config{Lanes: []config.LaneDef{
		{Name: "smith", Role: "worker"},
		{Name: "scout", Role: "forge-smith"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := resolveHoldLane(registry, "worker", false)
	if err != nil || lane.Name != "smith" || lane.Role != "worker" {
		t.Fatalf("worker identity=%+v err=%v", lane, err)
	}
	id := lifecycle.HoldIdentity{Repository: "github.com/example/repo", Owner: lane.Role, Lane: lane.Name, Task: "FAC-69", Scope: "task"}
	if id.Repository == "" || id.Owner != "worker" || id.Lane != "smith" || id.Task != "FAC-69" || id.Scope != "task" {
		t.Fatalf("canonical task identity invalid: %+v", id)
	}
	if _, err := resolveHoldLane(registry, "unknown", false); err == nil {
		t.Fatal("unknown configured role unexpectedly resolved")
	}
	if _, err := resolveHoldLane(registry, "unknown", true); err == nil {
		t.Fatal("unknown explicit lane unexpectedly resolved")
	}
}

func TestNewLifecycleEngineFromConfigPreservesRosterAndStanding(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{
		{Name: "smith", Role: "worker", Standing: false},
		{Name: "scout", Role: "forge-smith", Standing: true},
	}}
	eng, err := newLifecycleEngineFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if eng.StandingRoster == nil || len(eng.Lanes) != 2 {
		t.Fatalf("production roster wiring lost configured lanes: %+v", eng)
	}
	smith, err := eng.StandingRoster.ResolveLiveAgentID("forge-smith")
	if err != nil || smith.Name != "smith" || smith.Role != "worker" || smith.Standing {
		t.Fatalf("non-standing smith roster entry=%+v err=%v", smith, err)
	}
	scout, err := eng.StandingRoster.ResolveRole("forge-smith")
	if err != nil || scout.Name != "scout" || !scout.Standing {
		t.Fatalf("standing scout roster entry=%+v err=%v", scout, err)
	}
}

func TestComposeHoldIdentityRejectsUnknownBeforeAuthorityComposition(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "smith", Role: "worker"}}}
	for _, tc := range []struct {
		name       string
		lane, task string
		scope      string
		explicit   bool
		owner      string
	}{
		{name: "unknown lane", lane: "unknown", task: "FAC-69", scope: "task", explicit: true, owner: "worker"},
		{name: "unknown role", lane: "unknown", scope: "lane"},
		{name: "missing lane", task: "FAC-69", scope: "task", explicit: true, owner: "worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary := &countingHoldBoundary{}
			opens := 0
			_, got, err := prepareHoldCommand(cfg, tc.lane, tc.task, tc.scope, tc.explicit, tc.owner, "repo", func() (holdAuthorityBoundary, error) {
				opens++
				return boundary, nil
			})
			if err == nil || got != nil || opens != 0 || boundary.generationReads != 0 || boundary.holds != 0 || boundary.releases != 0 {
				t.Fatalf("invalid target crossed effect boundary: err=%v got=%v opens=%d reads=%d holds=%d releases=%d", err, got, opens, boundary.generationReads, boundary.holds, boundary.releases)
			}
		})
	}
	boundary := &countingHoldBoundary{}
	opens := 0
	identity, got, err := prepareHoldCommand(cfg, "smith", "FAC-69", "task", true, "worker", "repo", func() (holdAuthorityBoundary, error) {
		opens++
		return boundary, nil
	})
	if err != nil || got != boundary || opens != 1 || identity.Lane != "smith" || identity.Owner != "worker" {
		t.Fatalf("valid target did not reach opener exactly once: identity=%+v got=%v opens=%d err=%v", identity, got, opens, err)
	}
}
