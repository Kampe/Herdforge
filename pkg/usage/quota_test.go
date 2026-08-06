package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func parseFixture(t *testing.T) *UsageSnapshot {
	t.Helper()
	var snap UsageSnapshot
	if err := json.Unmarshal([]byte(fixtureJSON), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &snap
}

func freezeTime() time.Time {
	return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
}

// newTestEngine pins the engine clock to freezeTime so pace classification is
// reproducible. Without it these tests silently rot: the fixture's resetsAt
// timestamps recede into the past, elapsed% climbs, and pace collapses from
// overpace to onpace some days after the fixture was authored.
func newTestEngine() *QuotaEngine {
	e := NewQuotaEngine()
	e.Now = freezeTime
	return e
}

func TestComputeBinding_Claude(t *testing.T) {
	snap := parseFixture(t)
	prov := snap.Providers["claude"]
	now := freezeTime()

	bs := computeBinding(prov, nil, DefaultExhaustedPct, now)
	if bs == nil {
		t.Fatal("binding returned nil")
	}
	if bs.Window != "weekly" {
		t.Errorf("expected weekly as binding window, got %s", bs.Window)
	}
	if bs.Used != 80 {
		t.Errorf("expected 80%% used, got %f", bs.Used)
	}
	// 80% used with only ~7% elapsed -> overpace (pace >> 150)
	if bs.Class != BurnOverpace {
		t.Errorf("expected overpace (80%% used, ~7%% elapsed), got %s", bs.Class)
	}
	if bs.Available {
		t.Error("binding should not set Available until decorate")
	}
}

func TestComputeBinding_ClaudeSessionFull(t *testing.T) {
	snap := parseFixture(t)
	prov := snap.Providers["claude"]

	// Claude session is 0%% used in fixture — the r.Used == 0 guard returns nil
	resources := map[string]bool{"session": true}
	now := freezeTime()
	bs := computeBinding(prov, resources, DefaultExhaustedPct, now)
	if bs != nil {
		t.Errorf("expected nil for 0%% used resource, got %+v", bs)
	}
}

func TestComputeBinding_CodexExhausted(t *testing.T) {
	snap := parseFixture(t)
	prov := snap.Providers["codex"]
	now := freezeTime()

	bs := computeBinding(prov, nil, DefaultExhaustedPct, now)
	if bs == nil {
		t.Fatal("binding returned nil")
	}
	if bs.Class != BurnExhausted {
		t.Errorf("expected exhausted, got %s", bs.Class)
	}
	// Codex used=100, resets=2026-08-08, freeze=Aug 2 12:00 UTC
	// 7-day weekly, elapsed ~5.5d remaining of 7d = ~1.5d elapsed = ~21%%
	// pace = 100 / 21 * 100 = ~476
	if bs.Pace < 400 || bs.Pace > 550 {
		t.Errorf("expected pace ~476, got %d", bs.Pace)
	}
}

func TestDecorate(t *testing.T) {
	bs := &BurnState{Used: 50, Remaining: 50, Window: "5h", ResetsIn: "2h"}
	d := decorate(bs, false, "Max 20x", false, DefaultExhaustedPct)
	if !d.Available {
		t.Error("expected available")
	}
	if d.Reason != "ok" {
		t.Errorf("expected reason ok, got %s", d.Reason)
	}
	if d.Plan != "Max 20x" {
		t.Errorf("expected plan Max 20x, got %s", d.Plan)
	}
}

func TestDecorate_Stale(t *testing.T) {
	bs := &BurnState{Used: 50, Remaining: 50}
	d := decorate(bs, true, "", false, DefaultExhaustedPct)
	if d.Available {
		t.Error("stale should not be available")
	}
	if d.Reason != "stale" {
		t.Errorf("expected reason stale, got %s", d.Reason)
	}
}

func TestDecorate_Nil(t *testing.T) {
	d := decorate(nil, false, "", false, DefaultExhaustedPct)
	if d.Available {
		t.Error("nil binding should not be available")
	}
	if d.Reason != "no-quota-data" {
		t.Errorf("expected reason no-quota-data, got %s", d.Reason)
	}
}

func TestDecorate_Exhausted(t *testing.T) {
	bs := &BurnState{Used: 96, Remaining: 4}
	d := decorate(bs, false, "", false, DefaultExhaustedPct)
	if d.Available {
		t.Error("exhausted should not be available")
	}
	if d.Reason != "exhausted" {
		t.Errorf("expected reason exhausted, got %s", d.Reason)
	}
}

func TestClassPace(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	cls, pace, pressure := classPace(30, WindowWeekly, "2026-08-09T00:00:00Z", DefaultExhaustedPct, now)
	// weekly: resets 2026-08-09 -> 7d window, freeze Aug 2 12:00 = 6.5d remaining
	// elapsed = 0.5d / 7d * 100 = ~7.1%%, capped to 4%% min
	// pace = 30 / 7.1 * 100 = ~420
	if cls != BurnOverpace {
		t.Errorf("expected overpace (30%% in ~7%% elapsed -> pace ~420), got %s", cls)
	}
	if pace < 380 || pace > 460 {
		t.Errorf("expected pace ~420, got %d", pace)
	}
	// pressure = max(30, min(420, 250)/2) = max(30, 125) = 125
	if pressure != 125 {
		t.Errorf("expected pressure 125, got %f", pressure)
	}

	// Fast burn in 5h window: 40% used in 1h elapsed
	cls2, pace2, _ := classPace(40, Window5h, "2026-08-02T17:00:00Z", DefaultExhaustedPct, now)
	if cls2 != BurnOverpace {
		t.Errorf("expected overpace for fast burn, got %s", cls2)
	}
	if pace2 < 150 {
		t.Errorf("expected pace >= 150 for fast burn, got %d", pace2)
	}

	cls3, _, _ := classPace(50, Window5h, "", DefaultExhaustedPct, now)
	if cls3 != BurnUntracked {
		t.Errorf("expected untracked when no resetsAt, got %s", cls3)
	}
}

func TestPoolResources(t *testing.T) {
	snap := parseFixture(t)

	pools := poolResources("claude", snap.Providers["claude"])
	if pools == nil {
		t.Fatal("pools is nil")
	}
	if _, ok := pools["default"]; !ok {
		t.Error("expected default pool for claude")
	}
	if _, ok := pools["fable"]; !ok {
		t.Error("expected fable pool for claude")
	}

	codexPools := poolResources("codex", snap.Providers["codex"])
	if _, ok := codexPools["default"]; !ok {
		t.Error("expected default pool for codex")
	}

	// Antigravity with no resources
	antigrav := ProviderUsage{Resources: map[string]ResourceUsage{}}
	agPools := poolResources("antigravity", antigrav)
	if agPools == nil {
		t.Fatal("antigravity pools is nil")
	}
	defaultPool, ok := agPools["all"]
	if !ok || len(defaultPool) != 0 {
		t.Errorf("expected empty all pool, got %v", defaultPool)
	}
}

func TestComputeAll(t *testing.T) {
	snap := parseFixture(t)
	e := newTestEngine()
	computed := e.ComputeAll(snap)

	claude, ok := computed["claude"]
	if !ok {
		t.Fatal("missing claude in computed")
	}
	if !claude.Available {
		t.Error("claude should be available at 80%% (under 95 exhausted)")
	}
	if claude.Class != BurnOverpace {
		t.Errorf("claude class: expected overpace, got %s", claude.Class)
	}
	if len(claude.Pools) == 0 {
		t.Error("claude should have pools")
	}

	codex, ok := computed["codex"]
	if !ok {
		t.Fatal("missing codex in computed")
	}
	if codex.Available {
		t.Error("codex should not be available at 100%")
	}
	if codex.Reason != "exhausted" {
		t.Errorf("codex reason: expected exhausted, got %s", codex.Reason)
	}
}

func TestPickProvider_SelectsClaude(t *testing.T) {
	snap := parseFixture(t)
	e := newTestEngine()
	computed := e.ComputeAll(snap)

	pick, state, err := e.PickProvider(computed, []string{"claude", "codex"})
	if err != nil {
		t.Fatalf("pick error: %v", err)
	}
	// Codex is exhausted (100%%), so must pick claude
	if pick != "claude" {
		t.Errorf("expected claude, got %s", pick)
	}
	if !state.Available {
		t.Errorf("picked state should be available, got reason=%s", state.Reason)
	}
}

func TestPickProvider_AllExhausted(t *testing.T) {
	e := newTestEngine()
	exhausted := map[string]BurnState{
		"codex":  {Available: false, Reason: "exhausted", Used: 100},
		"claude": {Available: false, Reason: "exhausted", Used: 100},
	}
	_, _, err := e.PickProvider(exhausted, []string{"codex", "claude"})
	if err == nil {
		t.Fatal("expected error when all exhausted")
	}
}

func TestPickProvider_NoAmong(t *testing.T) {
	snap := parseFixture(t)
	e := newTestEngine()
	computed := e.ComputeAll(snap)

	pick, _, err := e.PickProvider(computed, nil)
	if err != nil {
		t.Fatalf("pick error: %v", err)
	}
	if pick == "" {
		t.Fatal("expected a pick with default among")
	}
}

func TestProviderOK(t *testing.T) {
	snap := parseFixture(t)
	e := newTestEngine()
	computed := e.ComputeAll(snap)

	_, ok := e.ProviderOK(computed, "claude")
	if !ok {
		t.Errorf("claude should be OK at 80%%")
	}
	_, ok = e.ProviderOK(computed, "codex")
	if ok {
		t.Errorf("codex should NOT be OK at 100%%")
	}
	_, ok = e.ProviderOK(computed, "nonexistent")
	if ok {
		t.Errorf("nonexistent should not be OK")
	}
}

func TestAliasProvider(t *testing.T) {
	e := newTestEngine()
	if e.AliasProvider("agy") != "antigravity" {
		t.Errorf("agy -> antigravity failed")
	}
	if e.AliasProvider("claude") != "claude" {
		t.Errorf("claude -> claude failed")
	}
}

func TestHumanDuration(t *testing.T) {
	if humanDuration(-1*time.Second) != "now" {
		t.Errorf("expected now for negative, got %s", humanDuration(-1*time.Second))
	}
	if humanDuration(30*time.Minute) != "30m" {
		t.Errorf("expected 30m, got %s", humanDuration(30*time.Minute))
	}
	if humanDuration(90*time.Minute) != "1h30m" {
		t.Errorf("expected 1h30m, got %s", humanDuration(90*time.Minute))
	}
	if humanDuration(48*time.Hour) != "2d" {
		t.Errorf("expected 2d, got %s", humanDuration(48*time.Hour))
	}
}

func TestComputeAll_NilSnapshot(t *testing.T) {
	e := newTestEngine()
	computed := e.ComputeAll(nil)
	if computed != nil {
		t.Error("nil snapshot should return nil")
	}
}

func TestComputeBinding_NoWindows(t *testing.T) {
	prov := ProviderUsage{
		Resources: map[string]ResourceUsage{
			"credits": {Kind: "balance", Unit: "credits", Available: 100},
		},
	}
	bs := computeBinding(prov, nil, DefaultExhaustedPct, time.Now())
	if bs != nil {
		t.Error("no real windows should return nil")
	}
}

func TestComputeBinding_UntrackedWindow(t *testing.T) {
	prov := ProviderUsage{
		Resources: map[string]ResourceUsage{
			"session": {
				Kind: "consumption", Unit: "percent",
				Used: 50, WindowSeconds: 18000,
			},
		},
	}
	bs := computeBinding(prov, nil, DefaultExhaustedPct, time.Now())
	if bs == nil {
		t.Fatal("binding should compute even without resetsAt")
	}
	if bs.Class != BurnUntracked {
		t.Errorf("expected untracked, got %s", bs.Class)
	}
}
