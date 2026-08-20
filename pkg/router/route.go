package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
	"github.com/Kampe/Herdforge/pkg/credits"
	"github.com/Kampe/Herdforge/pkg/posture"
	"github.com/Kampe/Herdforge/pkg/toolpolicy"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// Port of bin/herd-route (chainseer): pick the healthy execution surface for a
// task shape. The decision tables below are VERBATIM from the zsh script —
// tests/route parity depends on them not drifting. Selection is:
//
//	rank = effective_pressure*100 + candidate_fit*fit_weight*100 + pref
//
// with a pressure comfort floor, shape-scoped fit weights, coordinator
// runway-first ranking, codex spark / agy gemini pool fallbacks, and the
// lazer absolute-last-resort precedence (CHA-206).

// Shapes admitted by the router.
var validShapes = map[string]bool{
	"coordinator": true, "architecture": true, "implementation": true,
	"research": true, "bounded": true, "advisory": true,
	"qa-light": true, "qa": true, "adversarial": true,
}

// AllShapes is every task shape Waterfall accepts. Kept next to Waterfall so
// the two cannot drift.
func AllShapes() []string {
	return []string{"coordinator", "architecture", "implementation", "research",
		"bounded", "advisory", "qa-light", "qa", "adversarial"}
}

// Waterfall returns the preference-ordered candidate providers for a shape.
func Waterfall(shape string) ([]string, error) {
	switch shape {
	case "coordinator":
		// Coordinator identity is stable across provider failover. Keep the
		// preferred Codex/Claude surfaces first, but include Grok as a real
		// native fallback when a five-hour or weekly Claude window is spent.
		return []string{"codex", "claude", "grok"}, nil
	case "architecture":
		return []string{"claude", "agy", "codex", "grok", "ollama", "lazer"}, nil
	case "implementation":
		return []string{"claude", "grok", "codex", "ollama", "agy", "lazer"}, nil
	case "research":
		return []string{"claude", "agy", "ollama", "grok", "codex", "lazer"}, nil
	case "bounded":
		return []string{"codex", "claude", "ollama", "grok", "agy", "lazer"}, nil
	case "advisory":
		return []string{"opencode", "ollama", "claude", "grok", "agy"}, nil
	case "qa-light":
		return []string{"codex", "claude", "ollama", "grok", "lazer"}, nil
	case "qa":
		return []string{"claude", "grok", "agy", "codex", "kimi", "ollama", "lazer"}, nil
	case "adversarial":
		return []string{"grok", "claude", "agy", "codex", "kimi", "ollama", "lazer"}, nil
	default:
		return nil, fmt.Errorf("herd-route: unknown task shape: %s", shape)
	}
}

// OllamaHeavyModel mirrors herd_ollama_heavy_model: glm default while
// kimi-k3 is extra-usage-only; HERD_OLLAMA_USE_KIMI=1 restores Kimi.
func OllamaHeavyModel() string {
	if os.Getenv("HERD_OLLAMA_USE_KIMI") == "1" {
		return "litellm/ollama/kimi-k3:cloud"
	}
	return "litellm/ollama/glm-5.2:cloud"
}

// ModelFor maps provider:shape to the exact model, "" meaning either
// model-default surface (kimi) or fail-closed no-model (agy:qa-light,
// opencode non-advisory) — callers distinguish via modelDefaultSurface.
func ModelFor(provider, shape string) string {
	switch provider + ":" + shape {
	case "claude:coordinator":
		return "claude-fable-5"
	case "claude:architecture", "claude:qa", "claude:adversarial":
		return "claude-opus-5"
	case "claude:qa-light", "claude:bounded":
		return "claude-haiku-4-5"
	}
	switch provider {
	case "claude":
		return "claude-sonnet-5"
	case "agy":
		switch shape {
		case "architecture", "implementation", "research", "bounded", "qa", "adversarial":
			return "claude-opus-4-6-thinking"
		case "qa-light", "coordinator":
			return "" // fail closed: no governed AGY model for these shapes
		}
		return "gemini-3.1-pro-high"
	case "kimi":
		return "" // model-default surface: exact argv is `kimi --auto`
	case "opencode":
		switch shape {
		case "advisory":
			return "opencode/deepseek-v4-pro"
		case "bounded":
			return "opencode/deepseek-v4-flash"
		case "qa-light":
			return "opencode/kimi-k3"
		}
		return "" // fail closed: direct catalog models are advisory-only
	case "codex":
		switch shape {
		case "coordinator":
			return "gpt-5.6-sol"
		case "architecture", "adversarial":
			return "gpt-5.6-terra"
		case "bounded", "qa-light":
			return "gpt-5.3-codex-spark"
		}
		return "gpt-5.6-luna"
	case "ollama":
		switch shape {
		case "architecture", "qa", "adversarial", "implementation":
			return OllamaHeavyModel()
		case "qa-light", "bounded":
			return "litellm/ollama/qwen3.5:cloud"
		}
		return "litellm/ollama/glm-5.2:cloud"
	case "grok":
		return "grok-4.6"
	case "lazer":
		switch shape {
		case "coordinator", "architecture":
			return "litellm/lazer/claude-fable-5"
		case "implementation":
			return "litellm/lazer/gpt-5.6-sol"
		case "research":
			return "litellm/lazer/kimi-k3"
		case "qa-light":
			return "litellm/lazer/qwen-3.7-plus"
		case "qa", "adversarial":
			return "litellm/lazer/grok-4.6"
		case "bounded":
			return "litellm/lazer/qwen-3.7-plus"
		}
		return "litellm/lazer/claude-sonnet-5"
	}
	return ""
}

// modelDefaultSurface: an empty model is the surface's own default, not a
// fail-closed hole.
func modelDefaultSurface(provider string) bool { return provider == "kimi" }

func effortValid(e string) bool {
	switch e {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// EffortFor mirrors effort_for: shape-tracked ladder with HERD_EFFORT_<SHAPE>
// overrides (invalid overrides warned and ignored).
//
// Worker launches use this shape ladder via EffortForRequest. Reviewer
// launches do NOT use shape defaults for effort — they go through
// EffortForRequest (medium by default; high only for final/critical R2/R3).
func EffortFor(shape string) string {
	key := "HERD_EFFORT_" + strings.ToUpper(strings.ReplaceAll(shape, "-", "_"))
	if ov := os.Getenv(key); ov != "" {
		if effortValid(ov) {
			return ov
		}
		fmt.Fprintf(os.Stderr, "herd-route: WARN ignoring invalid %s=%q (allowed: low|medium|high|xhigh|max)\n", key, ov)
	}
	switch shape {
	case "coordinator":
		return "medium"
	case "architecture":
		return "max"
	case "qa-light":
		return "low"
	case "adversarial":
		return "xhigh"
	case "bounded":
		return "low"
	}
	return "high"
}

// PeerEffort clamps to what codex/agy/grok CLIs accept (herd_peer_effort).
func PeerEffort(effort string) string {
	switch effort {
	case "xhigh", "max":
		return "high"
	case "low", "medium", "high":
		return effort
	}
	return "medium"
}

// FamilyFor mirrors family_for.
func FamilyFor(provider, model string) string {
	m := strings.ToLower(model)
	switch provider {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "grok":
		return "xai"
	case "kimi":
		return "moonshot"
	case "agy":
		switch {
		case strings.HasPrefix(m, "claude"):
			return "anthropic"
		case strings.HasPrefix(m, "gemini"):
			return "google"
		case strings.HasPrefix(m, "gpt"):
			return "open-weight"
		}
		return "antigravity"
	case "ollama":
		switch {
		case strings.Contains(m, "glm"):
			return "zhipu"
		case strings.Contains(m, "kimi"):
			return "moonshot"
		case strings.Contains(m, "qwen"):
			return "alibaba"
		case strings.Contains(m, "deepseek"):
			return "deepseek"
		}
		return "open-weight"
	case "opencode":
		switch {
		case strings.Contains(m, "deepseek"):
			return "deepseek"
		case strings.Contains(m, "kimi"):
			return "moonshot"
		}
		return "open-weight"
	case "lazer":
		switch {
		case strings.Contains(m, "claude"):
			return "anthropic"
		case strings.Contains(m, "gpt"):
			return "openai"
		case strings.Contains(m, "grok"):
			return "xai"
		case strings.Contains(m, "gemini"):
			return "google"
		case strings.Contains(m, "glm"):
			return "zhipu"
		case strings.Contains(m, "kimi"):
			return "moonshot"
		case strings.Contains(m, "qwen"):
			return "alibaba"
		}
		return "proxy"
	}
	return ""
}

// QuotaPoolFor mirrors quota_pool_for: independently metered pools.
func QuotaPoolFor(provider, model string) string {
	switch provider {
	case "claude":
		if model == "claude-fable-5" {
			return "fable"
		}
	case "agy":
		if strings.HasPrefix(strings.ToLower(model), "gemini") {
			return "gemini"
		}
		return "nonGemini"
	case "codex":
		if strings.Contains(model, "spark") {
			return "spark"
		}
	}
	return "default"
}

// AgyGeminiPoolFallback mirrors agy_gemini_pool_fallback.
func AgyGeminiPoolFallback(shape string) string {
	switch shape {
	case "architecture", "implementation", "research", "bounded", "qa", "adversarial":
		return "gemini-3.1-pro-high"
	}
	return ""
}

const PiHarness = "pi"

// IsVendorHarness reports whether a harness is a directly supported Herdr
// fleet surface. This is the launch-admission table: Herdr spawn validation,
// confinement, and router decisions must agree or a route can be READY while
// launch fails (Kimi is intentionally headless-only until Herdr advertises
// that kind). Pi remains available only for legacy router decisions.
func IsVendorHarness(harness string) bool {
	surface, ok := SurfaceFor(harness)
	return ok && surface.VendorHarness
}

// IsLaneLaunchable reports whether Herdr can create the harness kind for a
// standing/task lane. Headless-only surfaces such as Kimi remain available to
// `herd shot`, but must not be admitted into a Herdr lane until the Herdr
// server advertises that kind. Keeping this predicate beside IsVendorHarness
// prevents router READY state from outrunning launch capability.
func IsLaneLaunchable(provider string) bool {
	surface, ok := SurfaceFor(provider)
	return ok && surface.VendorHarness
}

// PiModelFor maps a routed provider/model to Pi's exact provider-qualified model.
func PiModelFor(provider, model string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	if provider == "" || model == "" {
		return "", fmt.Errorf("Pi harness requires provider and model")
	}
	strip := func(value string, prefixes ...string) string {
		for _, prefix := range prefixes {
			if strings.HasPrefix(value, prefix) {
				return value[len(prefix):]
			}
		}
		return value
	}
	requireSuffix := func(suffix, kind string) (string, error) {
		if strings.TrimSpace(suffix) == "" {
			return "", fmt.Errorf("unsupported Pi %s model %q", kind, model)
		}
		return suffix, nil
	}
	switch provider {
	case "codex":
		suffix, err := requireSuffix(strip(model, "openai-codex/", "codex/", "openai/"), "codex")
		if err != nil {
			return "", err
		}
		return "openai-codex/" + suffix, nil
	case "claude":
		suffix, err := requireSuffix(strip(model, "anthropic/", "claude/"), "claude")
		if err != nil {
			return "", err
		}
		return "anthropic/" + suffix, nil
	case "agy":
		mapped := strip(model, "google/")
		if mapped == "" || !strings.HasPrefix(mapped, "gemini-") {
			return "", fmt.Errorf("unsupported Pi agy model %q", model)
		}
		return "google/" + mapped, nil
	case "grok":
		suffix, err := requireSuffix(strip(model, "xai/", "grok/"), "grok")
		if err != nil {
			return "", err
		}
		return "xai/" + suffix, nil
	case "opencode":
		suffix, err := requireSuffix(strip(model, "opencode/"), "opencode")
		if err != nil {
			return "", err
		}
		return "opencode/" + suffix, nil
	case "lazer":
		const prefix = "litellm/lazer/"
		if !strings.HasPrefix(model, prefix) {
			return "", fmt.Errorf("unsupported Pi lazer model %q", model)
		}
		if _, err := requireSuffix(model[len(prefix):], "lazer"); err != nil {
			return "", err
		}
		return model, nil
	case "ollama":
		const prefix = "litellm/ollama/"
		if !strings.HasPrefix(model, prefix) {
			return "", fmt.Errorf("unsupported Pi ollama model %q", model)
		}
		if _, err := requireSuffix(model[len(prefix):], "ollama"); err != nil {
			return "", err
		}
		return model, nil
	default:
		return "", fmt.Errorf("unsupported Pi provider %q", provider)
	}
}

// PiBareModel undoes PiModelFor's vendor qualification, recovering the routed
// model from a Pi harness argv.
//
// Only the prefixes PiModelFor ADDS are stripped. opencode/ and litellm/… are
// the routed model's own name, which PiModelFor passes through untouched, so
// stripping them would invent a model the router never issued. Kept beside
// PiModelFor so the two lists cannot drift apart.
func PiBareModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"openai-codex/", "anthropic/", "google/", "xai/"} {
		if bare, ok := strings.CutPrefix(m, prefix); ok {
			return bare
		}
	}
	return m
}

// HarnessArgvFor returns the native harness and exact interactive argv. Pi is
// an optional legacy adapter only when HERD_USE_PI=1; no launch path requires
// it. Coordinators therefore run directly on Codex Sol or Claude Fable.
func HarnessArgvFor(provider, model, effort string) (string, []string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if surface, ok := SurfaceFor(provider); !ok || (!surface.VendorHarness && !useLegacyPi()) {
		return "", nil, fmt.Errorf("unsupported launch provider %q", provider)
	}
	// Native Herdr mode is the default in every environment. Set HERD_USE_PI=1
	// only for an existing deployment that intentionally retains the adapter.
	if !useLegacyPi() {
		argv := ArgvFor(provider, model, effort)
		if len(argv) == 0 {
			return "", nil, fmt.Errorf("no direct harness argv contract for %s/%s", provider, model)
		}
		return strings.ToLower(strings.TrimSpace(provider)), argv, nil
	}
	piModel, err := PiModelFor(provider, model)
	if err != nil {
		return "", nil, err
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	validEffort := map[string]bool{"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	if !validEffort[effort] {
		return "", nil, fmt.Errorf("unsupported Pi thinking level %q", effort)
	}
	return PiHarness, []string{PiHarness, "--model", piModel, "--thinking", effort}, nil
}

// ArgvFor mirrors argv_json: the exact launch argv per provider contract.
// opencode-family argv is launcher-owned (herd_opencode_persistent_argv);
// here we emit the canonical model invocation.
func ArgvFor(provider, model, effort string) []string {
	pe := PeerEffort(effort)
	switch provider {
	case "claude":
		var base []string
		if model == "claude-fable-5" || model == "claude-opus-5" {
			base = []string{"claude", "--model", model, "--effort", effort, "--fallback-model", "claude-sonnet-5"}
		} else {
			base = []string{"claude", "--model", model, "--effort", effort}
		}
		// FAC-173: Agent/Task tools must not be exposed on fleet Claude surfaces.
		compiled, err := agentpolicy.CompileClaudeArgs(base)
		if err != nil {
			return nil
		}
		return compiled
	case "agy":
		return []string{"agy", "--model", model, "--prompt-interactive"}
	case "codex":
		base := []string{"codex", "--model", model, "-c", "model_reasoning_effort=" + pe, "-a", "never", "-c", toolpolicy.CodexDisableCodeReviewGraph}
		// FAC-173: multi_agent collaboration must be disabled before start.
		compiled, err := agentpolicy.CompileCodexArgs(base)
		if err != nil {
			return nil
		}
		return compiled
	case "grok":
		return []string{"grok", "--model", model, "--reasoning-effort", pe, "--always-approve"}
	case "kimi":
		return []string{"kimi", "--auto"}
	case "ollama", "opencode", "lazer":
		// herd_opencode_persistent_argv contract: the launcher appends
		// --prompt <kickoff> (spilling >800-char kickoffs to a packet file)
		// and refuses lazer models without the HERD_LAZER_LAST_RESORT
		// handshake set from Route.LazerLastResort.
		return []string{"opencode", "--model", model, "--auto"}
	}
	return nil
}

// HeadlessArgvFor is the argv for a ONE-SHOT, non-interactive run.
//
// This is deliberately separate from ArgvFor. ArgvFor launches an interactive
// pane that herdr later sends text to; reusing it headlessly just printed the
// CLI's help, because none of these tools take a prompt on stdin in
// interactive mode. promptPath is a file containing the prompt; the bool
// reports whether the caller should ALSO pipe the prompt on stdin (surfaces
// that read stdin rather than a file).
func HeadlessArgvFor(provider, model, effort, promptPath string) (argv []string, delivery PromptDelivery) {
	pe := PeerEffort(effort)
	switch provider {
	case "grok":
		return []string{"grok", "--model", model, "--reasoning-effort", pe,
			"--prompt-file", promptPath, "--output-format", "plain"}, DeliverByFile
	case "agy":
		// agy --print takes the prompt as a POSITIONAL argument; piping it on
		// stdin is silently ignored and agy answers as if asked nothing.
		// --print must be the LAST flag: a flag after it consumes the
		// positional, which fails the same silent way. No permission flag —
		// a shot is read-only and does not need one.
		return []string{"agy", "--model", model, "--print"}, DeliverByArg
	case "claude":
		base := []string{"claude", "--model", model, "--effort", effort, "-p"}
		compiled, err := agentpolicy.CompileClaudeArgs(base)
		if err != nil {
			return nil, DeliverByStdin
		}
		return compiled, DeliverByStdin
	case "codex":
		base := []string{"codex", "exec", "--model", model,
			"-c", "model_reasoning_effort=" + pe, "-s", "read-only"}
		compiled, err := agentpolicy.CompileCodexArgs(base)
		if err != nil {
			return nil, DeliverByStdin
		}
		return compiled, DeliverByStdin
	case "kimi":
		return []string{"kimi", "--auto"}, DeliverByStdin
	case "ollama", "opencode", "lazer":
		return []string{"opencode", "run", "--model", model}, DeliverByStdin
	}
	return nil, DeliverByStdin
}

// HeadlessProviders is the canonical set of surfaces that have a
// HeadlessArgvFor contract — i.e. the providers a headless one-shot can use at
// all. It is the single source shot capability derives from, so a surface the
// router can route and launch headlessly (kimi) cannot be silently absent from
// a second, hand-maintained list the way it was before FAC-224.
//
// Keep this in lockstep with the switch above: every entry MUST yield a
// non-nil argv from HeadlessArgvFor, and every provider case in that switch
// MUST appear here. TestHeadlessProvidersMatchArgvContract enforces both.
func HeadlessProviders() []string {
	result := make([]string, 0, len(SurfaceCapabilities()))
	for _, surface := range SurfaceCapabilities() {
		if surface.Headless {
			result = append(result, surface.Provider)
		}
	}
	return result
}

// PromptDelivery is how a headless surface accepts its prompt. Getting this
// wrong is silent: agy ignores stdin entirely and answers as though it were
// asked nothing at all, which reads as a model failure rather than a wiring bug.
type PromptDelivery int

const (
	DeliverByStdin PromptDelivery = iota
	DeliverByFile
	DeliverByArg
)

// MaxArgPromptBytes bounds a prompt delivered as argv. Well under the ~1MB
// macOS ARG_MAX so a large review packet fails with a clear message instead of
// E2BIG or a truncated exec.
const MaxArgPromptBytes = 200 * 1024

// Route is the routing decision, mirroring herd-route's route_json payload.
type Route struct {
	Provider        string   `json:"provider"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort"`
	Family          string   `json:"family"`
	Task            string   `json:"task"`
	LazerLastResort bool     `json:"lazer_last_resort"`
	Availability    string   `json:"availability"`
	QuotaPool       string   `json:"quota_pool"`
	QuotaPressure   int      `json:"quota_pressure"`
	Score           int      `json:"score"`
	Reason          string   `json:"reason"`
	Argv            []string `json:"argv,omitempty"`
}

// Probes abstracts the live availability checks so tests are hermetic.
type Probes struct {
	// CLIPresent reports whether the required launch/probe surface CLI exists.
	CLIPresent func(cli string) bool
	// Launchable checks the exact provider/model tuple before a pane is created.
	Launchable func(provider, model string) (bool, string)
	// Now supplies the clock for cooldown expiry.
	Now func() time.Time
	// LiveCount reports the number of live agents using the exact routed
	// provider/model/pool tuple. A nil function preserves library callers that
	// do not have a live Herdr roster; production route commands install it.
	LiveCount func(provider, model, pool string) (int, error)
}

func defaultProbes() *Probes {
	return &Probes{
		CLIPresent: func(cli string) bool {
			_, err := exec.LookPath(cli)
			return err == nil
		},
		Launchable: func(provider, _ string) (bool, string) {
			surface, ok := SurfaceFor(provider)
			if !ok {
				return false, fmt.Sprintf("unsupported routed provider %q", provider)
			}
			// Kimi is intentionally not configured on this host. A present CLI is
			// not evidence of an account or usable provider, so keep it out of
			// advertised routes unless an operator explicitly enables it.
			if provider == "kimi" && os.Getenv("HERD_KIMI_ENABLED") != "1" {
				return false, "kimi provider is not configured"
			}
			return ProbeSurface(surface)
		},
		Now: time.Now,
	}
}

// SurfaceRouter picks execution surfaces using live quota + the verbatim tables.
type SurfaceRouter struct {
	Engine   *usage.QuotaEngine
	Computed map[string]usage.BurnState
	Probes   *Probes
}

// NewRouter builds a SurfaceRouter over a computed quota map (usage.ComputeAll).
func NewRouter(engine *usage.QuotaEngine, computed map[string]usage.BurnState) *SurfaceRouter {
	return &SurfaceRouter{Engine: engine, Computed: computed, Probes: defaultProbes()}
}

func cliFor(provider string) string {
	if !useLegacyPi() {
		return provider
	}
	switch provider {
	case "codex":
		return PiHarness
	case "ollama", "opencode", "lazer":
		return "opencode"
	}
	return provider
}

func localDirectHarness() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("HERD_MODE")))
	return mode == "local" || mode == "dev" || mode == "development"
}

func useLegacyPi() bool {
	value, present := os.LookupEnv("HERD_USE_PI")
	if !present {
		// Library callers retain the historical adapter until they explicitly
		// select native routing. cmd/herd sets HERD_USE_PI=0 at startup, so the
		// actual CLI never requires Pi.
		return true
	}
	return strings.TrimSpace(value) == "1"
}

// GlobalStateDir mirrors herdr_global_state_dir. Exported so writers of the
// cooldown store (the quota supervisor's --act) land files in the exact
// directory this package reads; a second path convention would write cools
// nobody enforces.
func GlobalStateDir() string {
	if d := os.Getenv("HERDR_ROUTE_STATE_DIR"); d != "" {
		return d
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "herdr", "routing")
}

// globalCooldownReason mirrors global_cooldown_reason: the global store is a
// peer authority (CHA-792); an unexpired scoped entry blocks the candidate.
func (r *SurfaceRouter) globalCooldownReason(provider, model, pool string) string {
	return CooldownReason(r.Probes.Now(), provider, model, pool)
}

// Cooldown is one unexpired entry from the routing cooldown store.
//
// Source is whatever wrote it ("" for hand-written holds). Readers that also
// WRITE cools need it: a writer that treats its own entry as independent
// evidence has built a latch it can never open.
type Cooldown struct {
	Reason    string
	Source    string
	ExpiresAt time.Time
}

// CooldownReason reports why an unexpired scoped cooldown blocks
// (provider, model, pool), or "" when nothing does.
func CooldownReason(at time.Time, provider, model, pool string) string {
	if c := CooldownFor(at, provider, model, pool); c != nil {
		return c.Reason
	}
	return ""
}

// CooldownFor returns the unexpired scoped cool blocking (provider, model,
// pool), or nil.
//
// Exported so the quota supervisor grades a surface against the same store,
// the same scoping rules, and the same expiry arithmetic that Pick enforces.
// A supervisor with its own reader would eventually disagree with the gate it
// exists to anticipate.
func CooldownFor(at time.Time, provider, model, pool string) *Cooldown {
	gdir := GlobalStateDir()
	paths := []string{filepath.Join(gdir, provider+".cooldown.json")}
	if pool != "" {
		paths = append(paths, filepath.Join(gdir, provider+"--"+pool+".cooldown.json"))
	}
	if model != "" {
		paths = append(paths, filepath.Join(gdir, provider+"--model--"+url.QueryEscape(model)+".cooldown.json"))
	}
	now := at.Unix()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e struct {
			ExpiresAt  int64  `json:"expiresAt"`
			ExpiresAt2 int64  `json:"expires_at"`
			Surface    string `json:"surface"`
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			Pool       string `json:"pool"`
			Reason     string `json:"reason"`
			Source     string `json:"source"`
		}
		if json.Unmarshal(data, &e) != nil {
			continue // a half-parsed file must not read as a cool
		}
		exp := e.ExpiresAt
		if exp == 0 {
			exp = e.ExpiresAt2
		}
		if exp <= now {
			continue
		}
		who := e.Surface
		if who == "" {
			who = e.Provider
		}
		if who != provider {
			continue
		}
		if e.Model != "" && e.Model != model {
			continue
		}
		if e.Pool != "" && e.Pool != pool {
			continue
		}
		reason := e.Reason
		if reason == "" {
			reason = "global cooldown"
		}
		return &Cooldown{Reason: reason, Source: e.Source, ExpiresAt: time.Unix(exp, 0).UTC()}
	}
	return nil
}

func csvHas(csv, item string) bool {
	for _, p := range strings.Split(csv, ",") {
		if strings.TrimSpace(p) == item {
			return true
		}
	}
	return false
}

// quotaState resolves the burn state for provider+pool. ollama/lazer meter
// through opencode. Missing quota data means an unmetered ledger surface and
// does not gate.
func (r *SurfaceRouter) quotaState(provider, pool string) (usage.BurnState, bool) {
	name := provider
	switch provider {
	case "ollama", "lazer":
		name = "opencode"
	}
	if r.Engine != nil {
		name = r.Engine.AliasProvider(name)
	}
	st, ok := r.Computed[name]
	if !ok {
		return usage.BurnState{}, false
	}
	if pool != "" && pool != "default" {
		if ps, ok := st.Pools[pool]; ok {
			return ps, true
		}
	}
	return st, true
}

// available mirrors provider_available's structural gates. Returns detail.
func (r *SurfaceRouter) available(provider, model, pool string) (bool, string) {
	if reason := r.globalCooldownReason(provider, model, pool); reason != "" {
		if provider == "agy" && strings.HasPrefix(strings.ToLower(model), "gemini") &&
			strings.Contains(strings.ToLower(reason), "quota") {
			// pool-scoped AGY quota cool must not suppress a healthy Gemini fallback
		} else {
			return false, "global cooldown: " + reason
		}
	}
	if os.Getenv("HERD_NO_GEMINI") != "" && os.Getenv("HERD_NO_GEMINI") != "0" &&
		provider == "agy" && strings.HasPrefix(strings.ToLower(model), "gemini") {
		return false, "no-gemini route guard blocks AGY Gemini models"
	}
	if model == "" && !modelDefaultSurface(provider) {
		return false, "no governed model for task shape"
	}
	if un := os.Getenv("HERD_UNAVAILABLE_PROVIDERS"); un != "" && csvHas(un, provider) {
		return false, "forced unavailable"
	}
	forcedAvailable := false
	if forced := os.Getenv("HERD_AVAILABLE_PROVIDERS"); forced != "" {
		if !csvHas(forced, provider) {
			return false, "not in forced availability set"
		}
		// Forced availability is an operator/test ranking seam, not proof that
		// the exact provider/model can execute. Keep the executable and provider
		// probes mandatory before advertising the surface.
		forcedAvailable = true
	}
	if !r.Probes.CLIPresent(cliFor(provider)) {
		return false, "CLI missing"
	}
	if r.Probes.Launchable != nil {
		launchable, reason := r.Probes.Launchable(provider, model)
		if !launchable {
			if strings.TrimSpace(reason) == "" {
				reason = "provider probe failed"
			}
			return false, "provider unavailable: " + reason
		}
	}
	if !forcedAvailable {
		if st, ok := r.quotaState(provider, pool); ok {
			if !st.Available && st.Reason != "no-quota-data" {
				return false, "quota " + st.Reason
			}
		}
	}
	if r.Probes.LiveCount != nil {
		live, err := r.Probes.LiveCount(provider, model, pool)
		if err != nil {
			return false, "live concurrency unknown: " + err.Error()
		}
		cap := 2
		if st, ok := r.quotaState(provider, pool); ok {
			cap = credits.ClassConcurrency(credits.PaceClass(st.Class))
		}
		if live >= cap {
			return false, fmt.Sprintf("at concurrency cap live=%d cap=%d", live, cap)
		}
		return true, fmt.Sprintf("available live=%d cap=%d", live, cap)
	}
	return true, "available"
}

func envSet(key string) bool {
	// Family posture is generation-fenced JSON (with env override). Prefer
	// familyPostureMode() at route entry points — this helper remains for
	// non-posture keys and for callers that only need a bool.
	switch key {
	case "HERD_CLAUDE_ONLY":
		mode, err := familyPostureMode()
		return err == nil && mode == posture.ModeClaudeOnly
	case "HERD_NO_CLAUDE":
		mode, err := familyPostureMode()
		return err == nil && mode == posture.ModeNoClaude
	}
	v := os.Getenv(key)
	return v != "" && v != "0"
}

// familyPostureMode is the single resolve path for claude-only / no-claude.
// Corrupt or contradictory posture fails closed (non-nil error).
func familyPostureMode() (posture.Mode, error) {
	mode, _, err := posture.Effective(context.Background())
	return mode, err
}

func fitWeightFor(shape string) int {
	if v := os.Getenv("HERD_ROUTE_FIT_WEIGHT"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	switch shape {
	case "qa", "adversarial":
		return 60
	case "qa-light":
		return 40
	}
	return 20
}

func pressureFloor() int {
	if v := os.Getenv("HERD_ROUTE_PRESSURE_FLOOR"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return 40
}

// Pick selects the execution surface for a shape, mirroring herd-route's
// scored-candidate loop, pool fallbacks, and lazer precedence.
func (r *SurfaceRouter) Pick(shape, requestedProvider, excludedFamily string) (*Route, error) {
	if !validShapes[shape] {
		return nil, fmt.Errorf("herd-route: unknown task shape: %s", shape)
	}

	mode, err := familyPostureMode()
	if err != nil {
		return nil, fmt.Errorf("herd-route: family posture: %w", err)
	}

	// Shared with Decide so the allowed provider set (waterfall, era
	// intersection, family posture) cannot diverge between them (FAC-102).
	candidates, err := r.candidateProviders(mode, shape, requestedProvider)
	if err != nil {
		return nil, err
	}

	fitWeight := fitWeightFor(shape)
	floor := pressureFloor()

	type scored struct {
		rank     int
		provider string
		model    string
		pressure int
		detail   string
	}
	var picks []scored
	modelOverride := map[string]string{}

	for pref, provider := range candidates {
		model := ModelFor(provider, shape)
		family := FamilyFor(provider, model)
		if excludedFamily != "" && family == excludedFamily {
			continue
		}
		// Family posture (provider+model+family) — applied before scoring so
		// proxy Anthropic and agy claude-* cannot sneak past a provider-only
		// filter under no-claude, and claude-only never scores a proxy surface.
		if ok, _ := posture.Allow(mode, provider, model, family); !ok {
			continue
		}
		pool := QuotaPoolFor(provider, model)
		ok, detail := r.available(provider, model, pool)

		// Spark pool fallback (CHA-973): an exhausted codex default pool
		// retries the independently metered spark model before abandoning codex.
		if !ok && provider == "codex" && model != "gpt-5.3-codex-spark" &&
			strings.Contains(detail, "exhausted") {
			if ok2, d2 := r.available("codex", "gpt-5.3-codex-spark", "spark"); ok2 {
				model = "gpt-5.3-codex-spark"
				modelOverride["codex"] = model
				pool = "spark"
				ok, detail = true, d2
			}
		}
		// AGY Gemini pool fallback: exhausted nonGemini must not strand work
		// when the Gemini pool is healthy.
		if !ok && provider == "agy" && !strings.HasPrefix(strings.ToLower(model), "gemini") &&
			(strings.Contains(detail, "exhausted") || strings.Contains(strings.ToLower(detail), "quota")) {
			if fb := AgyGeminiPoolFallback(shape); fb != "" {
				if ok2, d2 := r.available("agy", fb, "gemini"); ok2 {
					model = fb
					modelOverride["agy"] = fb
					pool = "gemini"
					ok, detail = true, d2
				}
			}
		}
		if !ok {
			continue
		}

		// FAC-142: surface at/over weekly cap loses to a healthy alternative.
		var st usage.BurnState
		var haveQuota bool
		st, haveQuota = r.quotaState(provider, pool)
		if haveQuota && weeklyAtOrOverCap(st) {
			continue
		}
		// Non-v4 deepseek is never a valid pick.
		if ForbiddenDeepSeek(model) {
			continue
		}

		pressure := effectivePressure(st, haveQuota)

		fit := pref
		if shape == "coordinator" {
			fit = 0
		}
		var rank int
		switch {
		case shape == "coordinator" && haveQuota && st.ExhaustsBeforeReset != nil && *st.ExhaustsBeforeReset && st.RunwayMinutes != nil:
			runway := *st.RunwayMinutes
			if runway > 100000 {
				runway = 100000
			}
			rank = (100000-runway)*100 + pref
		case shape == "coordinator" && haveQuota && st.ExhaustsBeforeReset != nil && !*st.ExhaustsBeforeReset:
			rank = pressure*100 + pref
		case shape == "coordinator":
			rank = 20000000 + pressure*100 + pref
		default:
			effective := pressure
			if effective < floor {
				effective = 0
			}
			rank = effective*100 + fit*fitWeight*100 + pref
		}
		picks = append(picks, scored{rank, provider, model, pressure, detail})
	}

	// Lazer precedence: any available non-lazer wins; lazer only when alone.
	// Claude-only already restricted candidates to native claude, so lazer
	// cannot be selected under that mode (proxy family is also forbidden).
	var nonLazer []scored
	for _, s := range picks {
		if s.provider != "lazer" {
			nonLazer = append(nonLazer, s)
		}
	}
	if len(nonLazer) > 0 {
		picks = nonLazer
	}

	if len(picks) == 0 {
		if mode == posture.ModeClaudeOnly {
			return nil, fmt.Errorf("herd-route: claude-only posture has no healthy native Claude route for task=%s", shape)
		}
		if mode == posture.ModeNoClaude {
			return nil, fmt.Errorf("herd-route: no-claude posture has no healthy non-Anthropic route for task=%s", shape)
		}
		return nil, fmt.Errorf("herd-route: no healthy provider for task=%s", shape)
	}

	sort.SliceStable(picks, func(i, j int) bool { return picks[i].rank < picks[j].rank })
	best := picks[0]

	model := best.model
	if ov, ok := modelOverride[best.provider]; ok {
		model = ov
	}
	effort := EffortFor(shape)
	reason := fmt.Sprintf("quota pressure + task-fit penalty (weight=%d)", fitWeight)
	if shape == "coordinator" {
		reason = "projected exhaustion runway; safe pools compare headroom"
	}
	if mode == posture.ModeClaudeOnly {
		reason = "claude-only posture; " + reason
	}
	if mode == posture.ModeNoClaude {
		reason = "no-claude posture (anthropic family held out); " + reason
	}

	return &Route{
		Provider:        best.provider,
		Model:           model,
		Effort:          effort,
		Family:          FamilyFor(best.provider, model),
		Task:            shape,
		LazerLastResort: best.provider == "lazer",
		Availability:    best.detail,
		QuotaPool:       QuotaPoolFor(best.provider, model),
		QuotaPressure:   best.pressure,
		Score:           best.rank,
		Reason:          reason,
		Argv:            ArgvFor(best.provider, model, effort),
	}, nil
}
