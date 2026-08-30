package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type UsageSnapshot struct {
	GeneratedAt       time.Time                `json:"generatedAt"`
	Schema            string                   `json:"schema,omitempty"`
	Providers         map[string]ProviderUsage `json:"providers"`
	ProviderErrors    map[string]string        `json:"providerErrors,omitempty"`
	QuotaSource       string                   `json:"quotaSource,omitempty"`
	QuotaHandoffError string                   `json:"quotaHandoffError,omitempty"`
}

const (
	QuotaSourceOpenUsage        = "openusage"
	QuotaSourceOpenUsageHandoff = "openusage-handoff"
	QuotaSourceNative           = "native"
)

const (
	defaultQuotaCommandTimeout = 30 * time.Second
	defaultQuotaHandoffMaxAge  = 2 * time.Minute
	quotaHandoffFutureSkew     = 30 * time.Second
)

var quotaNow = time.Now

type EntitlementKind string

const (
	EntitlementMetered   EntitlementKind = "metered"
	EntitlementUnmetered EntitlementKind = "unmetered"
)

type ProviderUsage struct {
	DisplayName string                   `json:"displayName"`
	Plan        string                   `json:"plan,omitempty"`
	Entitlement EntitlementKind          `json:"entitlement,omitempty"`
	Resources   map[string]ResourceUsage `json:"resources"`
	Stale       bool                     `json:"stale"`
}

type ResourceUsage struct {
	Kind          string  `json:"kind"`
	Limit         float64 `json:"limit,omitempty"`
	Remaining     float64 `json:"remaining,omitempty"`
	Available     float64 `json:"available,omitempty"`
	Unit          string  `json:"unit"`
	Used          float64 `json:"used,omitempty"`
	Utilization   float64 `json:"utilization,omitempty"`
	ResetsAt      string  `json:"resetsAt,omitempty"`
	WindowSeconds int     `json:"windowSeconds,omitempty"`
}

type grokAuthEntry struct {
	Key          string `json:"key"`
	RefreshToken string `json:"refresh_token"`
}

// FAC-684: this was `var openUsageBinary = findOpenUsageBinary()`, resolved once
// at package init. HERD_OPENUSAGE_BIN therefore had to be set before the process
// started; setting it later -- which is the only thing an in-process test can do
// -- silently had no effect, so every test that "stubbed" quota was in fact
// reading the machine's live numbers and passing or failing by time of day.
// Resolve on use instead. The lookup is a couple of stat calls.

func findOpenUsageBinary() string {
	if handoff := strings.TrimSpace(os.Getenv("HERD_QUOTA_HANDOFF_BIN")); handoff != "" {
		return handoff
	}
	if override := strings.TrimSpace(os.Getenv("HERD_OPENUSAGE_BIN")); override != "" {
		return override
	}
	paths := []string{
		"/Applications/OpenUsage.app/Contents/Helpers/openusage",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "openusage"
}

func FetchSnapshot() (*UsageSnapshot, error) {
	snap, binaryErr := fetchViaBinary("")
	if binaryErr == nil {
		if quotaHandoffRequired() {
			if err := validateQuotaHandoffSnapshot(snap, quotaNow()); err != nil {
				snap.QuotaHandoffError = "OpenUsage quota handoff rejected: " + err.Error()
			}
		}
		return snap, nil
	}
	snap, directErr := fetchDirectAll()
	if snap != nil && quotaHandoffRequired() {
		snap.QuotaHandoffError = fmt.Sprintf("OpenUsage quota handoff unavailable: %v", binaryErr)
		// The snapshot remains available to diagnostics so an operator can see
		// what this host polled, but the handoff marker makes every admission
		// consumer fail closed. It is not a provider outage.
		return snap, nil
	}
	return snap, directErr
}

func FetchProvider(provider string) (*UsageSnapshot, error) {
	snap, binaryErr := fetchViaBinary(provider)
	if binaryErr == nil {
		if quotaHandoffRequired() {
			if err := validateQuotaHandoffSnapshot(snap, quotaNow()); err != nil {
				snap.QuotaHandoffError = "OpenUsage quota handoff rejected: " + err.Error()
			}
		}
		return snap, nil
	}
	snap, directErr := fetchDirectProvider(provider)
	if snap != nil && quotaHandoffRequired() {
		snap.QuotaHandoffError = fmt.Sprintf("OpenUsage quota handoff unavailable: %v", binaryErr)
		return snap, nil
	}
	return snap, directErr
}

func fetchViaBinary(provider string) (*UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), quotaCommandTimeout())
	defer cancel()
	binary := findOpenUsageBinary()
	var cmd *exec.Cmd
	if provider == "" {
		cmd = exec.CommandContext(ctx, binary)
	} else {
		cmd = exec.CommandContext(ctx, binary, provider)
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("openusage exec timeout after %s: %w", quotaCommandTimeout(), ctx.Err())
		}
		return nil, fmt.Errorf("openusage exec: %w", err)
	}
	var snap UsageSnapshot
	if err := json.Unmarshal(out, &snap); err != nil {
		return nil, fmt.Errorf("openusage decode: %w", err)
	}
	snap.QuotaSource = QuotaSourceOpenUsage
	if strings.TrimSpace(os.Getenv("HERD_QUOTA_HANDOFF_BIN")) != "" {
		snap.QuotaSource = QuotaSourceOpenUsageHandoff
	}
	normalizeSnapshotProviderKeys(&snap)
	return &snap, nil
}

func quotaCommandTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HERD_QUOTA_COMMAND_TIMEOUT_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return defaultQuotaCommandTimeout
}

func quotaHandoffMaxAge() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HERD_QUOTA_HANDOFF_MAX_AGE_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return defaultQuotaHandoffMaxAge
}

func validateQuotaHandoffSnapshot(snap *UsageSnapshot, now time.Time) error {
	if snap == nil {
		return fmt.Errorf("snapshot is missing")
	}
	if snap.Schema != "openusage.limits.v1" {
		return fmt.Errorf("schema %q, want openusage.limits.v1", snap.Schema)
	}
	if snap.QuotaSource != QuotaSourceOpenUsage && snap.QuotaSource != QuotaSourceOpenUsageHandoff {
		return fmt.Errorf("source %q is diagnostic-only", snap.QuotaSource)
	}
	if snap.GeneratedAt.IsZero() {
		return fmt.Errorf("source generatedAt is missing")
	}
	age := now.Sub(snap.GeneratedAt)
	if age < -quotaHandoffFutureSkew {
		return fmt.Errorf("source generatedAt is %s in the future", (-age).Round(time.Second))
	}
	if age > quotaHandoffMaxAge() {
		return fmt.Errorf("source generatedAt is stale by %s (max %s)", age.Round(time.Second), quotaHandoffMaxAge())
	}
	for name, detail := range snap.ProviderErrors {
		if strings.Contains(detail, "ambiguous provider instances") {
			return fmt.Errorf("provider %s normalization is ambiguous: %s", name, detail)
		}
	}
	if len(snap.Providers) == 0 {
		return fmt.Errorf("snapshot has no unambiguous providers")
	}
	return nil
}

// quotaHandoffRequired is true on WSL review hosts, whose account quota must
// be handed off by OpenUsage rather than silently reconstructed from whatever
// credentials happen to exist on the remote machine. The explicit override
// extends the same contract to other remote hosts and keeps tests hermetic.
func quotaHandoffRequired() bool {
	if strings.TrimSpace(os.Getenv("HERD_QUOTA_HANDOFF_BIN")) != "" {
		return true
	}
	if raw, present := os.LookupEnv("HERD_QUOTA_HANDOFF_REQUIRED"); present {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "1", "true", "yes":
			return true
		default:
			return false
		}
	}
	if runtime.GOOS != "linux" {
		return false
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft")
}

func canonicalProviderKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if base, instance, ok := strings.Cut(key, "@"); ok && base != "" && instance != "" {
		key = base
	}
	switch key {
	case "agy", "gemini":
		return "antigravity"
	default:
		return key
	}
}

// normalizeSnapshotProviderKeys collapses OpenUsage instance-qualified keys
// such as claude@8f460da5 onto the canonical provider key used by routing.
// Two rows that collapse to one provider are ambiguous and become a named
// provider error instead of one account silently overwriting another.
func normalizeSnapshotProviderKeys(snap *UsageSnapshot) {
	if snap == nil {
		return
	}
	providers := make(map[string]ProviderUsage, len(snap.Providers))
	errorsByProvider := make(map[string]string, len(snap.ProviderErrors))
	origin := make(map[string]string, len(snap.Providers))
	collided := make(map[string]bool)

	providerNames := make([]string, 0, len(snap.Providers))
	for name := range snap.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	for _, rawName := range providerNames {
		name := canonicalProviderKey(rawName)
		if name == "" || collided[name] {
			continue
		}
		if previous, exists := origin[name]; exists {
			delete(providers, name)
			collided[name] = true
			errorsByProvider[name] = fmt.Sprintf("ambiguous provider instances %q and %q", previous, rawName)
			continue
		}
		origin[name] = rawName
		providers[name] = snap.Providers[rawName]
	}

	errorNames := make([]string, 0, len(snap.ProviderErrors))
	for name := range snap.ProviderErrors {
		errorNames = append(errorNames, name)
	}
	sort.Strings(errorNames)
	for _, rawName := range errorNames {
		name := canonicalProviderKey(rawName)
		if name == "" {
			continue
		}
		detail := snap.ProviderErrors[rawName]
		if previous := errorsByProvider[name]; previous != "" && previous != detail {
			detail = previous + "; " + detail
		}
		errorsByProvider[name] = detail
	}
	snap.Providers = providers
	snap.ProviderErrors = errorsByProvider
}

func normalizedSnapshot(snap *UsageSnapshot) *UsageSnapshot {
	if snap == nil {
		return nil
	}
	copy := *snap
	normalizeSnapshotProviderKeys(&copy)
	return &copy
}

// nativePollers is the full set of providers pkg/usage can poll without the
// OpenUsage macOS helper. Grok was the only entry until FAC-229; claude, codex
// and gemini fell back to shelling the app, so on a host without it those pools
// reported nothing at all.
var nativePollers = map[string]func() (ProviderUsage, error){
	"grok":   grokPoll,
	"claude": claudePoll,
	"codex":  codexPoll,
	"gemini": geminiPoll,
}

func fetchDirectAll() (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt:    time.Now(),
		Providers:      make(map[string]ProviderUsage),
		ProviderErrors: make(map[string]string),
		QuotaSource:    QuotaSourceNative,
	}
	// One provider's failure must not blank the others: a missing credential or
	// an expired token is normal on a machine that does not use that harness.
	// Absent beats fabricated — a zero-utilization entry would read as "plenty
	// of quota" and route work at a surface that is actually spent.
	for name, poll := range nativePollers {
		p, err := poll()
		if err != nil {
			snap.ProviderErrors[name] = err.Error()
			continue
		}
		p.Stale = false
		snap.Providers[name] = p
	}
	if len(snap.Providers) == 0 {
		return snap, fmt.Errorf("no provider could be polled natively")
	}
	normalizeSnapshotProviderKeys(snap)
	return snap, nil
}

func fetchDirectProvider(provider string) (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers:   make(map[string]ProviderUsage),
		QuotaSource: QuotaSourceNative,
	}
	name := canonicalProviderKey(provider)
	pollName := name
	if name == "antigravity" {
		pollName = "gemini"
	}
	poll, ok := nativePollers[pollName]
	if !ok {
		return nil, fmt.Errorf("direct polling not available for provider %q", provider)
	}
	p, err := poll()
	if err != nil {
		return nil, err
	}
	p.Stale = false
	snap.Providers[name] = p
	normalizeSnapshotProviderKeys(snap)
	return snap, nil
}

func grokPoll() (ProviderUsage, error) {
	home, _ := os.UserHomeDir()
	authPath := filepath.Join(home, ".grok", "auth.json")
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("grok auth.json: %w", err)
	}
	var authMap map[string]grokAuthEntry
	if err := json.Unmarshal(raw, &authMap); err != nil {
		return ProviderUsage{}, fmt.Errorf("grok auth decode: %w", err)
	}
	var token string
	for _, entry := range authMap {
		token = entry.Key
		break
	}
	if token == "" {
		return ProviderUsage{}, fmt.Errorf("grok auth.json: no token found")
	}
	return grokPollWithURL("https://cli-chat-proxy.grok.com/v1/billing?format=credits", token)
}

func grokPollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("grok billing request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", pollUserAgent)

	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("grok billing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, fmt.Errorf("grok billing: HTTP %d", resp.StatusCode)
	}

	var billing struct {
		Plan      string          `json:"plan"`
		Total     *float64        `json:"total"`
		Used      *float64        `json:"used"`
		Remaining *float64        `json:"remaining"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&billing); err != nil {
		return ProviderUsage{}, fmt.Errorf("grok billing decode: %w", err)
	}
	if rawError := strings.TrimSpace(string(billing.Error)); rawError != "" && rawError != "null" {
		return ProviderUsage{}, fmt.Errorf("grok billing: response carried an error")
	}
	if billing.Total == nil || billing.Used == nil || billing.Remaining == nil {
		return ProviderUsage{}, fmt.Errorf("grok billing: response carried no metering or flat-rate entitlement")
	}
	total, used, remaining := *billing.Total, *billing.Used, *billing.Remaining
	if total < 0 || used < 0 || remaining < 0 || (total == 0 && (used != 0 || remaining != 0)) || (total > 0 && used > total) {
		return ProviderUsage{}, fmt.Errorf("grok billing: response carried invalid metering values")
	}
	plan := strings.TrimSpace(billing.Plan)
	if total == 0 {
		// A successful authenticated response with the complete zero-total shape
		// is the Grok flat-rate contract. It is entitlement evidence, not a 0/0
		// consumption window: there is no percentage, reset, or temporal window
		// to report or invent.
		return ProviderUsage{
			DisplayName: "Grok",
			Plan:        plan,
			Entitlement: EntitlementUnmetered,
			Resources:   map[string]ResourceUsage{},
		}, nil
	}

	utilization := used / total

	return ProviderUsage{
		DisplayName: "Grok",
		Plan:        plan,
		Entitlement: EntitlementMetered,
		Resources: map[string]ResourceUsage{
			"billing": {
				Kind:        "consumption",
				Limit:       total,
				Remaining:   remaining,
				Unit:        "credits",
				Used:        used,
				Utilization: utilization,
			},
		},
	}, nil
}

func (s *UsageSnapshot) Utilization(name string) float64 {
	if s == nil {
		return 0
	}
	p, ok := s.Providers[canonicalProviderKey(name)]
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
	if s.QuotaHandoffError != "" {
		return false
	}
	name = canonicalProviderKey(name)
	p, ok := s.Providers[name]
	if !ok || p.Stale {
		return false
	}
	if _, failed := s.ProviderErrors[name]; failed {
		return false
	}
	if p.Entitlement == EntitlementUnmetered {
		return true
	}
	for _, r := range p.Resources {
		if r.Kind == "consumption" {
			return s.Utilization(name) < threshold
		}
	}
	return false
}
