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
	rearmCalls                                                  int
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
func (b *adapterBoundary) Hold(_ context.Context, identity lifecycle.HoldIdentity, actor, reason, code string, generation int64, expires *time.Time) (lifecycle.HoldRecord, error) {
	b.holdCalls++
	b.lastGeneration = generation
	return lifecycle.HoldRecord{HoldIdentity: identity, Actor: actor, Reason: reason, Code: code, Generation: generation, ExpiresAt: expires, Held: true}, b.holdErr
}
func (b *adapterBoundary) Release(_ context.Context, identity lifecycle.HoldIdentity, actor, reason, code string, generation int64) (lifecycle.HoldRecord, error) {
	b.releaseCalls++
	b.lastGeneration = generation
	return lifecycle.HoldRecord{HoldIdentity: identity, Actor: actor, Reason: reason, Code: code, Generation: generation, Held: false}, b.releaseErr
}
func (b *adapterBoundary) ReleaseAndRearm(_ context.Context, identity lifecycle.HoldIdentity, actor, reason string, generation int64) (lifecycle.LoopState, error) {
	b.rearmCalls++
	b.lastGeneration = generation
	return lifecycle.LoopState{HoldIdentity: identity, Mode: lifecycle.LoopRunning, Goal: "goal", Wakeup: "wakeup"}, b.releaseErr
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

func TestExecuteHoldCommandRejectsInvalidTargetBeforeAuthorityOpen(t *testing.T) {
	cases := []struct {
		name string
		edit func(*holdCommandRequest)
	}{
		{name: "invalid scope", edit: func(req *holdCommandRequest) { req.Scope = "bogus" }},
		{name: "unknown lane", edit: func(req *holdCommandRequest) { req.LaneValue = "unknown"; req.ExplicitLane = true }},
		{name: "unknown role", edit: func(req *holdCommandRequest) {
			req.LaneValue = "unknown"
			req.Scope = "lane"
			req.Task = ""
			req.ExplicitLane = false
		}},
		{name: "missing task", edit: func(req *holdCommandRequest) { req.Task = "" }},
		{name: "missing owner", edit: func(req *holdCommandRequest) { req.Owner = "" }},
		{name: "padded task owner", edit: func(req *holdCommandRequest) { req.Owner = " worker" }},
		{name: "padded lane owner", edit: func(req *holdCommandRequest) { req.Scope = "lane"; req.Task = ""; req.Owner = "worker " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boundary := &adapterBoundary{}
			opens := 0
			encoded := []any{}
			flushes := 0
			req := adapterRequest("on")
			tc.edit(&req)
			if err := executeHoldCommand(context.Background(), req, adapterDeps(boundary, "", &opens, &encoded, &flushes)); err == nil {
				t.Fatal("invalid target unexpectedly succeeded")
			}
			if opens != 0 || boundary.currentErr != nil || boundary.holdCalls != 0 || boundary.releaseCalls != 0 || len(encoded) != 0 || flushes != 0 {
				t.Fatalf("invalid target crossed effect boundary: opens=%d holds=%d releases=%d encoded=%d flushes=%d", opens, boundary.holdCalls, boundary.releaseCalls, len(encoded), flushes)
			}
		})
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
		switch receipt := encoded[0].(type) {
		case map[string]any:
			if action != "status" || receipt["repository"] != "github.com/example/repo" || receipt["owner"] != "worker" || receipt["lane"] != "smith" || receipt["task"] != "FAC-203" || receipt["generation"] != int64(1) || receipt["held"] != false || receipt["reason"] != "maintenance" || receipt["code"] != "operator_hold" {
				t.Fatalf("incomplete status receipt=%#v", receipt)
			}
		case lifecycle.HoldRecord:
			wantCode, wantHeld := "operator_release", false
			if action == "on" {
				wantCode, wantHeld = "operator_hold", true
			}
			if receipt.Repository != "github.com/example/repo" || receipt.Owner != "worker" || receipt.Lane != "smith" || receipt.Task != "FAC-203" || receipt.Scope != "task" || receipt.Generation != 1 || receipt.Actor != "tester" || receipt.Reason != "maintenance" || receipt.Code != wantCode || receipt.Held != wantHeld {
				t.Fatalf("incomplete %s receipt=%+v", action, receipt)
			}
		default:
			t.Fatalf("unexpected %s receipt type %T", action, encoded[0])
		}
	}
}

func TestExecuteHoldCommandLaneScopeDefaultsOwnerToConfiguredRole(t *testing.T) {
	boundary := &adapterBoundary{hasCurrent: false}
	opens := 0
	encoded := []any{}
	flushes := 0
	req := adapterRequest("on")
	req.Scope = "lane"
	req.Task = ""
	req.Owner = ""
	if err := executeHoldCommand(context.Background(), req, adapterDeps(boundary, "", &opens, &encoded, &flushes)); err != nil {
		t.Fatal(err)
	}
	if opens != 1 || boundary.closeCalls != 1 || boundary.holdCalls != 1 || len(encoded) != 1 || flushes != 1 {
		t.Fatalf("lane success counts: opens=%d closes=%d holds=%d receipts=%d flushes=%d", opens, boundary.closeCalls, boundary.holdCalls, len(encoded), flushes)
	}
	receipt, ok := encoded[0].(lifecycle.HoldRecord)
	if !ok || receipt.Repository != "github.com/example/repo" || receipt.Owner != "worker" || receipt.Lane != "smith" || receipt.Task != "" || receipt.Scope != "lane" || receipt.Generation != 1 || !receipt.Held {
		t.Fatalf("lane receipt did not contain configured owner/lane identity: %#v", encoded[0])
	}
}

func TestExecuteHoldCommandLaneReleaseUsesAtomicRearm(t *testing.T) {
	boundary := &adapterBoundary{hasCurrent: true}
	opens := 0
	encoded := []any{}
	flushes := 0
	req := adapterRequest("off")
	req.Scope = "lane"
	req.Task = ""
	req.Owner = ""
	if err := executeHoldCommand(context.Background(), req, adapterDeps(boundary, "", &opens, &encoded, &flushes)); err != nil {
		t.Fatal(err)
	}
	if boundary.rearmCalls != 1 || boundary.releaseCalls != 0 {
		t.Fatalf("lane release calls: rearm=%d release=%d", boundary.rearmCalls, boundary.releaseCalls)
	}
	state, ok := encoded[0].(lifecycle.LoopState)
	if !ok || state.Mode != lifecycle.LoopRunning || state.Goal != "goal" || state.Wakeup != "wakeup" {
		t.Fatalf("lane release receipt = %#v", encoded[0])
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
