package usage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type UsageSnapshot struct {
	GeneratedAt time.Time                `json:"generatedAt"`
	Providers   map[string]ProviderUsage `json:"providers"`
	// Errors carries machine-readable failure reasons ("<code>: <detail>") for
	// providers that were attempted and failed. A provider present in neither
	// map was not attempted and is UNKNOWN — absence never means zero usage or
	// healthy capacity.
	Errors map[string]string `json:"errors,omitempty"`
}

type ProviderUsage struct {
	DisplayName string `json:"displayName"`
	Plan        string `json:"plan,omitempty"`
	// Source names the authority that produced this reading, so a consumer can
	// distinguish native provider evidence from anything else.
	Source     string                   `json:"source,omitempty"`
	Account    *AccountIdentity         `json:"account,omitempty"`
	ObservedAt time.Time                `json:"observedAt"`
	Status     string                   `json:"status,omitempty"`
	Resources  map[string]ResourceUsage `json:"resources"`
	Stale      bool                     `json:"stale"`
}

type ResourceUsage struct {
	Kind          string  `json:"kind"`
	State         string  `json:"state,omitempty"` // active, not-started, or untracked
	Model         string  `json:"model,omitempty"`
	Pool          string  `json:"pool,omitempty"`
	Limit         float64 `json:"limit"`
	Remaining     float64 `json:"remaining"`
	Available     float64 `json:"available,omitempty"`
	Unit          string  `json:"unit"`
	Used          float64 `json:"used"`
	Utilization   float64 `json:"utilization"`
	ResetsAt      string  `json:"resetsAt,omitempty"`
	WindowSeconds int     `json:"windowSeconds"`
}

type grokAuthEntry struct {
	Key          string `json:"key"`
	RefreshToken string `json:"refresh_token"`
}

func FetchSnapshot() (*UsageSnapshot, error) {
	return fetchDirectAll()
}

func FetchProvider(provider string) (*UsageSnapshot, error) {
	return fetchDirectProvider(provider)
}

// FetchProviderForce is explicit for callers exposing --force. Single-provider
// polling has no cache layer, so force is intentionally a no-op after being
// wired through the public seam.
func FetchProviderForce(provider string, force bool) (*UsageSnapshot, error) {
	return fetchProviderCached(provider, force)
}

// nativePollers is the full set of providers pkg/usage polls directly. Every
// harness the fleet routes for has a native poller; there is no helper-binary
// path anywhere.
var nativePollers = map[string]func() (ProviderUsage, error){
	"grok":        grokPoll,
	"claude":      claudePoll,
	"codex":       codexPoll,
	"gemini":      geminiPoll,
	"antigravity": antigravityPoll,
	"litellm":     litellmPoll,
	"opencode":    opencodePoll,
	"kimi":        kimiPoll,
}

var nativePollerOverride struct {
	sync.RWMutex
	pollers map[string]func() (ProviderUsage, error)
}

// SetNativePollersForTest injects hermetic native acquisition fixtures and
// returns a restore function. It exists for cross-package production-path
// tests; callers must never use it to override live acquisition.
func SetNativePollersForTest(pollers map[string]func() (ProviderUsage, error)) func() {
	copyPollers := make(map[string]func() (ProviderUsage, error), len(pollers))
	for name, poll := range pollers {
		copyPollers[name] = poll
	}
	nativePollerOverride.Lock()
	old := nativePollerOverride.pollers
	nativePollerOverride.pollers = copyPollers
	nativePollerOverride.Unlock()
	return func() {
		nativePollerOverride.Lock()
		nativePollerOverride.pollers = old
		nativePollerOverride.Unlock()
	}
}

func activeNativePollers() map[string]func() (ProviderUsage, error) {
	nativePollerOverride.RLock()
	defer nativePollerOverride.RUnlock()
	if nativePollerOverride.pollers != nil {
		return nativePollerOverride.pollers
	}
	return nativePollers
}

// providerSource names the native authority per provider.
var providerSource = map[string]string{
	"claude":      "native:api.anthropic.com/api/oauth/usage",
	"codex":       "native:chatgpt.com/backend-api/wham/usage",
	"gemini":      "native:cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota",
	"grok":        "native:cli-chat-proxy.grok.com/v1/billing?format=credits",
	"antigravity": "native:same-host-language-server/RetrieveUserQuotaSummary",
	"litellm":     "native:authenticated-key-info",
	"opencode":    "native:opencode.ai/zen/go/v1/usage",
	"kimi":        "native:unsupported-no-quota-endpoint",
}

// decorateProvider attaches the reading's provenance: which native endpoint
// produced it and which authenticated account it belongs to.
func decorateProvider(name string, p ProviderUsage) ProviderUsage {
	p.Stale = false
	p.Source = providerSource[name]
	if p.Account == nil && name != "claude" {
		p.Account = providerAccountIdentity(name)
	}
	if p.ObservedAt.IsZero() {
		p.ObservedAt = time.Now().UTC()
	}
	return p
}

func fetchDirectAll() (*UsageSnapshot, error) {
	return fetchDirectAllWithPollers(activeNativePollers())
}

func fetchDirectAllWithPollers(pollers map[string]func() (ProviderUsage, error)) (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers:   make(map[string]ProviderUsage),
	}
	// Poll in parallel, each with its own 10-second deadline, so one slow or
	// hung provider cannot stretch a full fetch to four deadlines' worth of
	// waiting (measured at 29-272s serially before FAC-679's cache). A
	// provider's failure must not blank the others: a missing credential or an
	// expired token is normal on a machine that does not use that harness, and
	// the failure is recorded in Errors instead. Absent beats fabricated — a
	// zero-utilization entry would read as "plenty of quota" and route work at
	// a surface that is actually spent.
	type pollResult struct {
		name string
		pu   ProviderUsage
		err  error
	}
	results := make(chan pollResult, len(pollers))
	for name, poll := range pollers {
		go func(name string, poll func() (ProviderUsage, error)) {
			pu, err := poll()
			results <- pollResult{name: name, pu: pu, err: err}
		}(name, poll)
	}
	for range pollers {
		r := <-results
		if r.err != nil {
			if snap.Errors == nil {
				snap.Errors = map[string]string{}
			}
			snap.Errors[r.name] = classifyPollError(r.err)
			continue
		}
		snap.Providers[r.name] = decorateProvider(r.name, r.pu)
	}
	if len(snap.Providers) == 0 {
		return snap, fmt.Errorf("no provider could be polled natively")
	}
	return snap, nil
}

func fetchDirectProvider(provider string) (*UsageSnapshot, error) {
	return fetchDirectProviderWithPollers(provider, activeNativePollers())
}

func fetchDirectProviderWithPollers(provider string, pollers map[string]func() (ProviderUsage, error)) (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers:   make(map[string]ProviderUsage),
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	switch name {
	case "agy":
		name = "antigravity"
	case "lazer":
		name = "litellm"
	}
	poll, ok := pollers[name]
	if !ok {
		snap.Errors = map[string]string{name: classifyPollError(pollErrf("unsupported", "direct polling not available for provider %q", name))}
		return snap, pollErrf("unsupported", "direct polling not available for provider %q", name)
	}
	p, err := poll()
	if err != nil {
		snap.Errors = map[string]string{name: classifyPollError(err)}
		return snap, err
	}
	snap.Providers[name] = decorateProvider(name, p)
	return snap, nil
}

func grokPoll() (ProviderUsage, error) {
	home, _ := os.UserHomeDir()
	authPath := filepath.Join(home, ".grok", "auth.json")
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return ProviderUsage{}, pollErrf("auth-missing", "grok auth.json: %v; run grok login", err)
	}
	var authMap map[string]grokAuthEntry
	if err := json.Unmarshal(raw, &authMap); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "grok auth decode: %v", err)
	}
	keys := make([]string, 0, len(authMap))
	for k := range authMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ProviderUsage{}, pollErrf("auth-missing", "grok auth.json: no token found; run grok login")
	}
	if len(keys) > 1 {
		// Multiple profiles make both the token and the account ambiguous;
		// picking one by map order would attribute quota to a random account.
		return ProviderUsage{}, pollErrf("auth-ambiguous", "grok auth.json carries %d entries; refusing to attribute quota to an arbitrary account", len(keys))
	}
	token := authMap[keys[0]].Key
	if token == "" {
		return ProviderUsage{}, pollErrf("auth-missing", "grok auth.json: no token found; run grok login")
	}
	return grokPollWithURL("https://cli-chat-proxy.grok.com/v1/billing?format=credits", token)
}

// grokBilling accepts both documented shapes of the credits billing response:
// the upstream config shape (creditUsagePercent/currentPeriod, proto3-as-JSON
// where a zero percent is omitted) and the legacy credits-count shape
// (total/used/remaining). Only a WEEKLY period maps to the weekly pool — an
// account still on a monthly-only period reports no weekly window at all
// (unknown), never a mislabeled one.
type grokBilling struct {
	Config *struct {
		CreditUsagePercent float64 `json:"creditUsagePercent"`
		CurrentPeriod      *struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		OnDemandCap *struct {
			Val float64 `json:"val"`
		} `json:"onDemandCap"`
	} `json:"config"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

func grokPollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "opencode/1.0")

	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, netPollError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("grok billing", resp)
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("grok billing", resp.StatusCode)
	}
	var billing grokBilling
	if err := json.NewDecoder(resp.Body).Decode(&billing); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "grok billing decode: %v", err)
	}

	if billing.Config != nil && billing.Config.CurrentPeriod != nil {
		period := billing.Config.CurrentPeriod
		if !strings.Contains(period.Type, "WEEKLY") {
			return ProviderUsage{}, pollErrf("no-windows",
				"grok billing: current period %q is not weekly; no weekly window to report", period.Type)
		}
		used := billing.Config.CreditUsagePercent // proto3: omitted means a genuine zero
		res := ResourceUsage{
			Kind:          "consumption",
			Limit:         100,
			Remaining:     100 - used,
			Unit:          "percent",
			Used:          used,
			Utilization:   used / 100,
			ResetsAt:      period.End,
			WindowSeconds: 7 * 24 * 3600,
		}
		return ProviderUsage{DisplayName: "Grok", Resources: map[string]ResourceUsage{"weekly": res}}, nil
	}

	if billing.Total > 0 {
		utilization := billing.Used / billing.Total
		return ProviderUsage{
			DisplayName: "Grok",
			Resources: map[string]ResourceUsage{
				"weekly": {
					Kind:        "consumption",
					Limit:       billing.Total,
					Remaining:   billing.Remaining,
					Unit:        "percent",
					Used:        billing.Used,
					Utilization: utilization,
				},
			},
		}, nil
	}

	// A credits response that proves no window (all-zero counts, no config) is
	// indistinguishable from absent data; reporting it as 0% used would read
	// as healthy capacity.
	return ProviderUsage{}, pollErrf("no-windows", "grok billing: response carried no usable quota window")
}

func (s *UsageSnapshot) Utilization(name string) float64 {
	if s == nil {
		return 0
	}
	p, ok := s.Providers[name]
	if !ok {
		return 0
	}
	var totalUtil, count float64
	for _, r := range p.Resources {
		if r.Kind == "consumption" {
			totalUtil += r.Utilization
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return totalUtil / count
}

func (s *UsageSnapshot) HasCapacity(name string, threshold float64) bool {
	if s == nil {
		return false
	}
	return s.Utilization(name) < threshold
}
