package router

import (
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// clearRouteEnv isolates tests from operator postures and real cooldown state.
func clearRouteEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HERD_CLAUDE_ONLY", "HERD_NO_CLAUDE", "HERD_NO_GEMINI",
		"HERD_ERA_PROVIDERS", "HERD_AVAILABLE_PROVIDERS", "HERD_UNAVAILABLE_PROVIDERS",
		"HERD_ROUTE_FIT_WEIGHT", "HERD_ROUTE_PRESSURE_FLOOR", "HERD_OLLAMA_USE_KIMI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir()) // empty: no global cooldowns
}

func testRouter(computed map[string]usage.BurnState, clis ...string) *SurfaceRouter {
	present := map[string]bool{}
	for _, c := range clis {
		present[c] = true
	}
	r := NewRouter(usage.NewQuotaEngine(), computed)
	r.Probes = &Probes{
		CLIPresent: func(cli string) bool { return present[cli] },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

// The verbatim tables from bin/herd-route. If one of these fails, the port
// has drifted from the shell contract.
func TestModelForTable(t *testing.T) {
	clearRouteEnv(t)
	cases := map[[2]string]string{
		{"claude", "coordinator"}:      "claude-fable-5",
		{"claude", "architecture"}:     "claude-opus-5",
		{"claude", "qa"}:               "claude-opus-5",
		{"claude", "adversarial"}:      "claude-opus-5",
		{"claude", "qa-light"}:         "claude-haiku-4-5",
		{"claude", "bounded"}:          "claude-haiku-4-5",
		{"claude", "implementation"}:   "claude-sonnet-5",
		{"claude", "research"}:         "claude-sonnet-5",
		{"agy", "implementation"}:      "claude-opus-4-6-thinking",
		{"agy", "qa"}:                  "claude-opus-4-6-thinking",
		{"agy", "qa-light"}:            "",
		{"agy", "coordinator"}:         "",
		{"agy", "advisory"}:            "gemini-3.1-pro-high",
		{"kimi", "qa"}:                 "",
		{"opencode", "advisory"}:       "opencode/deepseek-v4-pro",
		{"opencode", "bounded"}:        "opencode/deepseek-v4-flash",
		{"opencode", "qa-light"}:       "opencode/kimi-k3",
		{"opencode", "implementation"}: "",
		{"codex", "coordinator"}:       "gpt-5.6-sol",
		{"codex", "architecture"}:      "gpt-5.6-terra",
		{"codex", "adversarial"}:       "gpt-5.6-terra",
		{"codex", "bounded"}:           "gpt-5.3-codex-spark",
		{"codex", "qa-light"}:          "gpt-5.3-codex-spark",
		{"codex", "implementation"}:    "gpt-5.6-luna",
		{"codex", "research"}:          "gpt-5.6-luna",
		{"codex", "qa"}:                "gpt-5.6-luna",
		{"ollama", "implementation"}:   "litellm/ollama/glm-5.2:cloud",
		{"ollama", "qa"}:               "litellm/ollama/glm-5.2:cloud",
		{"ollama", "qa-light"}:         "litellm/ollama/qwen3.5:cloud",
		{"ollama", "bounded"}:          "litellm/ollama/qwen3.5:cloud",
		{"grok", "qa"}:                 "grok-4.5",
		{"lazer", "coordinator"}:       "litellm/lazer/claude-fable-5",
		{"lazer", "architecture"}:      "litellm/lazer/claude-fable-5",
		{"lazer", "implementation"}:    "litellm/lazer/gpt-5.6-sol",
		{"lazer", "research"}:          "litellm/lazer/kimi-k3",
		{"lazer", "qa-light"}:          "litellm/lazer/qwen-3.7-plus",
		{"lazer", "bounded"}:           "litellm/lazer/qwen-3.7-plus",
		{"lazer", "qa"}:                "litellm/lazer/grok-4.5",
		{"lazer", "adversarial"}:       "litellm/lazer/grok-4.5",
	}
	for k, want := range cases {
		if got := ModelFor(k[0], k[1]); got != want {
			t.Errorf("ModelFor(%s, %s) = %q, want %q", k[0], k[1], got, want)
		}
	}
}

func TestOllamaKimiToggle(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_OLLAMA_USE_KIMI", "1")
	if got := ModelFor("ollama", "qa"); got != "litellm/ollama/kimi-k3:cloud" {
		t.Errorf("HERD_OLLAMA_USE_KIMI=1: got %q", got)
	}
}

func TestEffortForTable(t *testing.T) {
	clearRouteEnv(t)
	cases := map[string]string{
		"coordinator": "medium", "architecture": "max", "qa-light": "low",
		"adversarial": "xhigh", "bounded": "low",
		"implementation": "high", "research": "high", "qa": "high", "advisory": "high",
	}
	for shape, want := range cases {
		if got := EffortFor(shape); got != want {
			t.Errorf("EffortFor(%s) = %q, want %q", shape, got, want)
		}
	}
	t.Setenv("HERD_EFFORT_BOUNDED", "xhigh")
	if got := EffortFor("bounded"); got != "xhigh" {
		t.Errorf("env override ignored: got %q", got)
	}
	t.Setenv("HERD_EFFORT_BOUNDED", "bogus")
	if got := EffortFor("bounded"); got != "low" {
		t.Errorf("invalid override must fall through to ladder: got %q", got)
	}
}

func TestWaterfallTables(t *testing.T) {
	clearRouteEnv(t)
	want := map[string][]string{
		"coordinator":    {"codex", "claude"},
		"architecture":   {"claude", "agy", "codex", "grok", "ollama", "lazer"},
		"implementation": {"claude", "grok", "codex", "ollama", "agy", "lazer"},
		"research":       {"claude", "agy", "ollama", "grok", "codex", "lazer"},
		"bounded":        {"codex", "claude", "ollama", "grok", "agy", "lazer"},
		"advisory":       {"opencode", "ollama", "claude", "grok", "agy"},
		"qa-light":       {"codex", "claude", "ollama", "grok", "lazer"},
		"qa":             {"claude", "grok", "agy", "codex", "kimi", "ollama", "lazer"},
		"adversarial":    {"grok", "claude", "agy", "codex", "kimi", "ollama", "lazer"},
	}
	for shape, exp := range want {
		got, err := Waterfall(shape)
		if err != nil {
			t.Fatalf("Waterfall(%s): %v", shape, err)
		}
		if len(got) != len(exp) {
			t.Fatalf("Waterfall(%s) = %v, want %v", shape, got, exp)
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Errorf("Waterfall(%s)[%d] = %s, want %s", shape, i, got[i], exp[i])
			}
		}
	}
	if _, err := Waterfall("nonsense"); err == nil {
		t.Error("unknown shape must error")
	}
}

func TestQuotaPoolFor(t *testing.T) {
	cases := []struct{ p, m, want string }{
		{"claude", "claude-fable-5", "fable"},
		{"claude", "claude-sonnet-5", "default"},
		{"agy", "gemini-3.1-pro-high", "gemini"},
		{"agy", "claude-opus-4-6-thinking", "nonGemini"},
		{"codex", "gpt-5.3-codex-spark", "spark"},
		{"codex", "gpt-5.6-luna", "default"},
		{"grok", "grok-4.5", "default"},
	}
	for _, c := range cases {
		if got := QuotaPoolFor(c.p, c.m); got != c.want {
			t.Errorf("QuotaPoolFor(%s, %s) = %q, want %q", c.p, c.m, got, c.want)
		}
	}
}

func TestPickPreferenceOrderWins(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "claude" || route.Model != "claude-sonnet-5" || route.Effort != "high" {
		t.Fatalf("want claude/claude-sonnet-5/high, got %+v", route)
	}
}

func TestPickNoClaudeFilters(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_NO_CLAUDE", "1")
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider == "claude" {
		t.Fatal("no-claude posture must drop native claude")
	}
	if route.Provider != "grok" {
		t.Fatalf("want grok (next in waterfall), got %s", route.Provider)
	}
}

func TestPickClaudeOnlyHoists(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_CLAUDE_ONLY", "1")
	// bounded lists codex first; claude-only must hoist claude to win
	r := testRouter(nil, "claude", "codex", "opencode", "grok")
	route, err := r.Pick("bounded", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "claude" {
		t.Fatalf("claude-only must pick claude, got %s", route.Provider)
	}
}

func TestPickLazerLastResort(t *testing.T) {
	clearRouteEnv(t)
	// Only opencode CLI present: lazer/ollama/opencode all route through it.
	// implementation: ollama model exists, opencode fail-closed empty, lazer
	// has a model. ollama is available non-lazer -> must beat lazer.
	r := testRouter(nil, "opencode")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "ollama" {
		t.Fatalf("available non-lazer must beat lazer, got %s", route.Provider)
	}
	if route.LazerLastResort {
		t.Fatal("non-lazer pick must not be flagged last-resort")
	}
}

func TestPickLazerOnlyWhenAlone(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_AVAILABLE_PROVIDERS", "lazer")
	r := testRouter(nil, "opencode")
	route, err := r.Pick("research", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "lazer" || !route.LazerLastResort {
		t.Fatalf("want lazer flagged last-resort, got %+v", route)
	}
}

func TestPickQuotaExhaustionSkips(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {Available: false, Reason: "exhausted", Pressure: 100},
		"grok":   {Available: true, Pressure: 10},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "grok" {
		t.Fatalf("exhausted claude must be skipped, got %s", route.Provider)
	}
}

func TestPickPressureFloorKeepsPreference(t *testing.T) {
	clearRouteEnv(t)
	// claude at 28% (below floor 40) must beat grok at 5%: below the comfort
	// floor every provider is equally "free" and task fit governs.
	computed := map[string]usage.BurnState{
		"claude": {Available: true, Pressure: 28},
		"grok":   {Available: true, Pressure: 5},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "claude" {
		t.Fatalf("pressure floor must keep preference on claude, got %s", route.Provider)
	}
}

func TestPickReviewFitWeightDominates(t *testing.T) {
	clearRouteEnv(t)
	// qa fit weight 60: the first healthy listed reviewer wins even against a
	// slightly fresher pool above the floor.
	computed := map[string]usage.BurnState{
		"claude": {Available: true, Pressure: 45},
		"grok":   {Available: true, Pressure: 41},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("qa", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	// claude: 45*100+0 = 4500; grok: 41*100+1*60*100+1 = 10101 -> claude
	if route.Provider != "claude" || route.Model != "claude-opus-5" {
		t.Fatalf("qa fit weight must keep first healthy reviewer, got %+v", route)
	}
}

func TestPickEraIntersection(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_ERA_PROVIDERS", "codex grok")
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "grok" {
		t.Fatalf("era set must exclude claude and keep waterfall order (grok before codex), got %s", route.Provider)
	}
}

func TestPickAgyGeminiPoolFallback(t *testing.T) {
	clearRouteEnv(t)
	rm := 120
	f := false
	computed := map[string]usage.BurnState{
		"antigravity": { // quota engine canonicalizes agy -> antigravity
			Available: false, Reason: "exhausted", Pressure: 100,
			Pools: map[string]usage.BurnState{
				"gemini":    {Available: true, Pressure: 10, ExhaustsBeforeReset: &f, RunwayMinutes: &rm},
				"nonGemini": {Available: false, Reason: "exhausted", Pressure: 100},
			},
		},
	}
	r := testRouter(computed, "agy")
	route, err := r.Pick("qa", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "agy" || route.Model != "gemini-3.1-pro-high" {
		t.Fatalf("want agy gemini fallback, got %+v", route)
	}
	if route.QuotaPool != "gemini" {
		t.Fatalf("fallback must bill the gemini pool, got %s", route.QuotaPool)
	}
}

func TestGlobalCooldownBlocks(t *testing.T) {
	clearRouteEnv(t)
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	writeCooldown(t, dir, "claude.cooldown.json",
		`{"provider":"claude","expiresAt":1800000600,"reason":"manual hold"}`)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider == "claude" {
		t.Fatal("unexpired global cooldown must block claude")
	}
}

func TestExpiredCooldownIgnored(t *testing.T) {
	clearRouteEnv(t)
	dir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", dir)
	writeCooldown(t, dir, "claude.cooldown.json",
		`{"provider":"claude","expiresAt":1799999999,"reason":"stale"}`)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider != "claude" {
		t.Fatalf("expired cooldown must not block, got %s", route.Provider)
	}
}

func TestArgvContracts(t *testing.T) {
	cases := []struct {
		p, m, e string
		want    []string
	}{
		{"claude", "claude-fable-5", "medium",
			[]string{"claude", "--model", "claude-fable-5", "--effort", "medium", "--fallback-model", "claude-sonnet-5"}},
		{"claude", "claude-sonnet-5", "high",
			[]string{"claude", "--model", "claude-sonnet-5", "--effort", "high"}},
		{"codex", "gpt-5.6-luna", "xhigh",
			[]string{"codex", "--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=high", "-a", "never"}},
		{"grok", "grok-4.5", "max",
			[]string{"grok", "--model", "grok-4.5", "--reasoning-effort", "high", "--always-approve"}},
		{"kimi", "", "high", []string{"kimi", "--auto"}},
		{"agy", "claude-opus-4-6-thinking", "high",
			[]string{"agy", "--model", "claude-opus-4-6-thinking", "--prompt-interactive"}},
	}
	for _, c := range cases {
		got := ArgvFor(c.p, c.m, c.e)
		if len(got) != len(c.want) {
			t.Fatalf("ArgvFor(%s) = %v, want %v", c.p, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("ArgvFor(%s)[%d] = %q, want %q", c.p, i, got[i], c.want[i])
			}
		}
	}
}

func writeCooldown(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFile(dir+"/"+name, content); err != nil {
		t.Fatal(err)
	}
}
