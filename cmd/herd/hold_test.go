package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

type adapterBoundary struct {
	currentErr, hasErr, checkErr, holdErr, releaseErr, closeErr error
	current                                                     int64
	hasCurrent                                                  bool
	holdCalls                                                   int
	releaseCalls                                                int
	checkCalls                                                  int
	closeCalls                                                  int
	lastGeneration                                              int64
}

func (b *adapterBoundary) Close() error {
	b.closeCalls++
	return b.closeErr
}
func (b *adapterBoundary) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	if b.current == 0 {
		b.current = 1
	}
	return b.current, b.currentErr
}
func (b *adapterBoundary) HasCurrent(context.Context, lifecycle.HoldIdentity) (bool, error) {
	return b.hasCurrent, b.hasErr
}
func (b *adapterBoundary) Check(_ context.Context, _ lifecycle.HoldIdentity, generation int64) (lifecycle.HoldDecision, error) {
	b.checkCalls++
	b.lastGeneration = generation
	return lifecycle.HoldDecision{Generation: generation, Reason: "maintenance", Code: "operator_hold"}, b.checkErr
}
func (b *adapterBoundary) Hold(_ context.Context, _ lifecycle.HoldIdentity, _ string, _ string, _ string, generation int64, _ *time.Time) (lifecycle.HoldRecord, error) {
	b.holdCalls++
	b.lastGeneration = generation
	return lifecycle.HoldRecord{Generation: generation}, b.holdErr
}
func (b *adapterBoundary) Release(_ context.Context, _ lifecycle.HoldIdentity, _ string, _ string, _ string, generation int64) (lifecycle.HoldRecord, error) {
	b.releaseCalls++
	b.lastGeneration = generation
	return lifecycle.HoldRecord{Generation: generation}, b.releaseErr
}

func adapterConfig() *config.Config {
	return &config.Config{Lanes: []config.LaneDef{{Name: "smith", Role: "worker"}}}
}

func adapterRequest(action string) holdCommandRequest {
	return holdCommandRequest{Config: adapterConfig(), LaneValue: "smith", Task: "FAC-203", Scope: "task", ExplicitLane: true, Owner: "worker", Action: action, Actor: "tester", Reason: "maintenance", Code: "operator_hold"}
}

func adapterDeps(boundary *adapterBoundary, repository string, opens *int, encoded *[]any, flushes *int) holdCommandDependencies {
	return holdCommandDependencies{
		AuthenticateRepository: func() (string, error) { return "github.com/example/repo", nil },
		OpenAuthority: func() (holdAuthorityBoundary, error) {
			(*opens)++
			return boundary, nil
		},
		Encode: func(value any) error {
			*encoded = append(*encoded, value)
			return nil
		},
		Flush: func() error { (*flushes)++; return nil },
		Now:   func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) },
	}
}

func TestExecuteHoldCommandRejectsInvalidBeforeAuthorityOpen(t *testing.T) {
	boundary := &adapterBoundary{}
	opens := 0
	encoded := []any{}
	flushes := 0
	for _, action := range []string{"", "bogus"} {
		req := adapterRequest(action)
		deps := adapterDeps(boundary, "", &opens, &encoded, &flushes)
		if err := executeHoldCommand(context.Background(), req, deps); err == nil {
			t.Fatalf("action %q unexpectedly succeeded", action)
		}
	}
	if opens != 0 || boundary.closeCalls != 0 || len(encoded) != 0 || flushes != 0 {
		t.Fatalf("invalid action crossed effect boundary: opens=%d closes=%d encoded=%d flushes=%d", opens, boundary.closeCalls, len(encoded), flushes)
	}
}

func TestExecuteHoldCommandRejectsForeignAndPaddedRepositoryBeforeOpen(t *testing.T) {
	for _, override := range []string{"github.com/other/repo", " github.com/example/repo"} {
		boundary := &adapterBoundary{}
		opens := 0
		encoded := []any{}
		flushes := 0
		req := adapterRequest("on")
		req.Repository = override
		if err := executeHoldCommand(context.Background(), req, adapterDeps(boundary, override, &opens, &encoded, &flushes)); err == nil {
			t.Fatalf("repository override %q unexpectedly succeeded", override)
		}
		if opens != 0 || boundary.holdCalls != 0 {
			t.Fatalf("repository override crossed authority boundary: opens=%d holds=%d", opens, boundary.holdCalls)
		}
	}
}

func TestExecuteHoldCommandRepositoryFailurePreventsOpen(t *testing.T) {
	repositoryErr := errors.New("repository identity unavailable")
	boundary := &adapterBoundary{}
	opens := 0
	encoded := []any{}
	flushes := 0
	deps := adapterDeps(boundary, "", &opens, &encoded, &flushes)
	deps.AuthenticateRepository = func() (string, error) { return "", repositoryErr }
	if err := executeHoldCommand(context.Background(), adapterRequest("on"), deps); !errors.Is(err, repositoryErr) || opens != 0 {
		t.Fatalf("repository failure crossed authority boundary: err=%v opens=%d", err, opens)
	}
}

func TestExecuteHoldCommandCheckFailurePreventsHold(t *testing.T) {
	checkErr := errors.New("check unavailable")
	boundary := &adapterBoundary{hasCurrent: true, checkErr: checkErr}
	opens := 0
	encoded := []any{}
	flushes := 0
	err := executeHoldCommand(context.Background(), adapterRequest("on"), adapterDeps(boundary, "", &opens, &encoded, &flushes))
	if !errors.Is(err, checkErr) || boundary.holdCalls != 0 || boundary.checkCalls != 1 {
		t.Fatalf("check failure did not fail closed: err=%v checks=%d holds=%d", err, boundary.checkCalls, boundary.holdCalls)
	}
}

func TestExecuteHoldCommandSuccessClosesOnceAndFlushesOneReceipt(t *testing.T) {
	for _, action := range []string{"status", "on", "off"} {
		boundary := &adapterBoundary{hasCurrent: false}
		opens := 0
		encoded := []any{}
		flushes := 0
		if err := executeHoldCommand(context.Background(), adapterRequest(action), adapterDeps(boundary, "", &opens, &encoded, &flushes)); err != nil {
			t.Fatalf("action %s: %v", action, err)
		}
		if opens != 1 || boundary.closeCalls != 1 || len(encoded) != 1 || flushes != 1 {
			t.Fatalf("action %s receipt/close counts: opens=%d closes=%d encoded=%d flushes=%d", action, opens, boundary.closeCalls, len(encoded), flushes)
		}
		if action == "on" && boundary.holdCalls != 1 || action == "off" && boundary.releaseCalls != 1 || action == "status" && boundary.checkCalls != 1 {
			t.Fatalf("action %s authority call counts: checks=%d holds=%d releases=%d", action, boundary.checkCalls, boundary.holdCalls, boundary.releaseCalls)
		}
	}
}

func TestExecuteHoldCommandPropagatesOutputFlushAndCloseErrors(t *testing.T) {
	flushErr := errors.New("flush failed")
	closeErr := errors.New("close failed")
	boundary := &adapterBoundary{hasCurrent: false, closeErr: closeErr}
	opens := 0
	encoded := []any{}
	flushes := 0
	deps := adapterDeps(boundary, "", &opens, &encoded, &flushes)
	deps.Flush = func() error { flushes++; return flushErr }
	err := executeHoldCommand(context.Background(), adapterRequest("on"), deps)
	if !errors.Is(err, flushErr) || !errors.Is(err, closeErr) {
		t.Fatalf("output/close errors were not joined: %v", err)
	}
	boundary = &adapterBoundary{hasCurrent: false}
	opens, encoded, flushes = 0, nil, 0
	deps = adapterDeps(boundary, "", &opens, &encoded, &flushes)
	encodeErr := errors.New("encode failed")
	deps.Encode = func(any) error { return encodeErr }
	err = executeHoldCommand(context.Background(), adapterRequest("on"), deps)
	if !errors.Is(err, encodeErr) || flushes != 0 || boundary.closeCalls != 1 {
		t.Fatalf("encode failure was not propagated safely: %v flushes=%d closes=%d", err, flushes, boundary.closeCalls)
	}

	primaryErr := errors.New("hold failed")
	boundary = &adapterBoundary{hasCurrent: false, holdErr: primaryErr, closeErr: closeErr}
	opens, encoded, flushes = 0, nil, 0
	err = executeHoldCommand(context.Background(), adapterRequest("on"), adapterDeps(boundary, "", &opens, &encoded, &flushes))
	if !errors.Is(err, primaryErr) || !errors.Is(err, closeErr) || flushes != 0 {
		t.Fatalf("primary/close errors were not joined: %v flushes=%d", err, flushes)
	}
}

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
