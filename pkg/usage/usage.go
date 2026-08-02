package usage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type UsageSnapshot struct {
	GeneratedAt time.Time                `json:"generatedAt"`
	Providers   map[string]ProviderUsage `json:"providers"`
}

type ProviderUsage struct {
	DisplayName string                   `json:"displayName"`
	Plan        string                   `json:"plan,omitempty"`
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

var openUsageBinary = findOpenUsageBinary()

func findOpenUsageBinary() string {
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
	snap, err := fetchViaBinary("")
	if err == nil {
		return snap, nil
	}
	return fetchDirectAll()
}

func FetchProvider(provider string) (*UsageSnapshot, error) {
	snap, err := fetchViaBinary(provider)
	if err == nil {
		return snap, nil
	}
	return fetchDirectProvider(provider)
}

func fetchViaBinary(provider string) (*UsageSnapshot, error) {
	var cmd *exec.Cmd
	if provider == "" {
		cmd = exec.Command(openUsageBinary)
	} else {
		cmd = exec.Command(openUsageBinary, provider)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("openusage exec: %w", err)
	}
	var snap UsageSnapshot
	if err := json.Unmarshal(out, &snap); err != nil {
		return nil, fmt.Errorf("openusage decode: %w", err)
	}
	return &snap, nil
}

func fetchDirectAll() (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers:   make(map[string]ProviderUsage),
	}
	grokP, err := grokPoll()
	if err == nil {
		grokP.Stale = false
	}
	snap.Providers["grok"] = grokP
	return snap, nil
}

func fetchDirectProvider(provider string) (*UsageSnapshot, error) {
	snap := &UsageSnapshot{
		GeneratedAt: time.Now(),
		Providers:   make(map[string]ProviderUsage),
	}
	switch strings.ToLower(provider) {
	case "grok":
		p, err := grokPoll()
		if err != nil {
			return nil, err
		}
		p.Stale = false
		snap.Providers["grok"] = p
	default:
		return nil, fmt.Errorf("direct polling not available for provider %q", provider)
	}
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
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "opencode/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("grok billing: %w", err)
	}
	defer resp.Body.Close()

	var billing struct {
		Total     float64 `json:"total"`
		Used      float64 `json:"used"`
		Remaining float64 `json:"remaining"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&billing); err != nil {
		return ProviderUsage{}, fmt.Errorf("grok billing decode: %w", err)
	}

	utilization := 0.0
	if billing.Total > 0 {
		utilization = billing.Used / billing.Total
	}

	return ProviderUsage{
		DisplayName: "Grok",
		Plan:        "SuperGrok Heavy",
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
		Stale: false,
	}, nil
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
