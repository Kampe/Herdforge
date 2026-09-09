package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Antigravity quota is a same-host language-server authority. The collector
// never starts AGY and never substitutes Gemini CLI data for these buckets.
type antigravitySummary struct {
	Groups []struct {
		Buckets []struct {
			BucketID          string  `json:"bucketId"`
			DisplayName       string  `json:"displayName"`
			Window            string  `json:"window"`
			RemainingFraction float64 `json:"remainingFraction"`
			ResetTime         string  `json:"resetTime"`
		} `json:"buckets"`
	} `json:"groups"`
}

var antigravityBuckets = map[string]string{
	"gemini-5h": "geminiSession", "gemini-weekly": "geminiWeekly",
	"3p-5h": "nonGeminiSession", "3p-weekly": "nonGeminiWeekly",
}

func antigravityPoll() (ProviderUsage, error) {
	d, err := discoverAntigravity()
	if err != nil {
		return ProviderUsage{}, err
	}
	for _, port := range d.Ports {
		for _, scheme := range []string{"https", "http"} {
			url := scheme + "://127.0.0.1:" + strconv.Itoa(port) + "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
			p, pollErr := antigravityPollWithURL(url, d.CSRF)
			if pollErr == nil {
				return p, nil
			}
		}
	}
	if d.ExtensionPort > 0 {
		url := "http://127.0.0.1:" + strconv.Itoa(d.ExtensionPort) + "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
		return antigravityPollWithURL(url, d.CSRF)
	}
	return ProviderUsage{}, pollErrf("unsupported", "antigravity language server has no usable listening port")
}

type antigravityDiscovery struct {
	CSRF          string
	Ports         []int
	ExtensionPort int
}

var discoverAntigravity = discoverAntigravityProcess

func discoverAntigravityProcess() (antigravityDiscovery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		return antigravityDiscovery{}, pollErrf("unreachable", "antigravity process discovery failed")
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		command := strings.Join(fields[1:], " ")
		lower := strings.ToLower(command)
		if !strings.Contains(lower, "language_server") && !strings.Contains(lower, "agy") {
			continue
		}
		if strings.Contains(lower, "--app_data_dir") && !strings.Contains(lower, "antigravity") && !strings.Contains(lower, "antigravity-ide") {
			continue
		}
		csrf := flagValue(fields, "--csrf_token")
		ext := 0
		if v := flagValue(fields, "--extension_server_port"); v != "" {
			ext, _ = strconv.Atoi(v)
		}
		ports := listeningPorts(ctx, fields[0])
		if len(ports) == 0 && ext == 0 {
			continue
		}
		return antigravityDiscovery{CSRF: csrf, Ports: ports, ExtensionPort: ext}, nil
	}
	return antigravityDiscovery{}, pollErrf("unsupported", "antigravity language server is not running")
}

func flagValue(fields []string, flag string) string {
	for i, field := range fields {
		if field == flag && i+1 < len(fields) {
			return fields[i+1]
		}
		if strings.HasPrefix(field, flag+"=") {
			return strings.TrimPrefix(field, flag+"=")
		}
	}
	return ""
}

func listeningPorts(ctx context.Context, pid string) []int {
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", pid).Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var ports []int
	for _, field := range strings.Fields(string(out)) {
		if i := strings.LastIndex(field, ":"); i >= 0 {
			if p, err := strconv.Atoi(strings.TrimSuffix(field[i+1:], "(LISTEN)")); err == nil && p > 0 && !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func antigravityPollWithURL(url, csrf string) (ProviderUsage, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader("{}"))
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-codeium-csrf-token", csrf)
	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, netPollError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("antigravity quota", resp.StatusCode)
	}
	var summary antigravitySummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "antigravity quota decode: %v", err)
	}
	resources := map[string]ResourceUsage{}
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			name, ok := antigravityBuckets[bucket.BucketID]
			if !ok || bucket.RemainingFraction < 0 || bucket.RemainingFraction > 1 {
				continue
			}
			remaining := bucket.RemainingFraction * 100
			resources[name] = ResourceUsage{Kind: "consumption", State: "active", Pool: strings.TrimSuffix(name, "Session"), Unit: "percent", Used: 100 - remaining, Remaining: remaining, Limit: 100, Utilization: 1 - bucket.RemainingFraction, ResetsAt: bucket.ResetTime}
		}
	}
	if len(resources) == 0 {
		return ProviderUsage{}, pollErrf("no-windows", "antigravity quota: no recognized usable buckets")
	}
	return ProviderUsage{DisplayName: "Antigravity", Resources: resources}, nil
}

// LiteLLM's key-info endpoint is an authority only when it returns an
// enforceable budget. An authenticated key without one is explicitly
// untracked, never unlimited or healthy quota.
type litellmKeyInfo struct {
	KeyName     string          `json:"key_name"`
	BudgetMax   *float64        `json:"budget_max"`
	BudgetSpent *float64        `json:"budget_spent"`
	MaxBudget   *float64        `json:"max_budget"`
	Spend       *float64        `json:"spend"`
	RPMLimit    *float64        `json:"rpm_limit"`
	MaxParallel *float64        `json:"max_parallel_requests"`
	Error       string          `json:"error"`
	Info        *litellmKeyInfo `json:"info"`
}

func litellmPoll() (ProviderUsage, error) {
	key := strings.TrimSpace(os.Getenv("LITELLM_OC_KEY"))
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("LITELLM_BASE_URL")), "/")
	if key == "" || base == "" {
		return ProviderUsage{}, pollErrf("auth-missing", "litellm self-key or configured base URL is unavailable")
	}
	return litellmPollWithURL(base+"/key/info", key)
}

func litellmPollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, netPollError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("litellm key info", resp.StatusCode)
	}
	var info litellmKeyInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "litellm key info decode: %v", err)
	}
	if strings.TrimSpace(info.Error) != "" {
		return ProviderUsage{}, pollErrf("provider-error", "litellm key info: %s", strings.TrimSpace(info.Error))
	}
	if info.Info != nil {
		if strings.TrimSpace(info.Info.Error) != "" {
			return ProviderUsage{}, pollErrf("provider-error", "litellm key info: %s", strings.TrimSpace(info.Info.Error))
		}
		info = *info.Info
	}
	account := strings.TrimSpace(info.KeyName)
	accountIdentity := identity("litellm", account, "litellm:key-info:key_name")
	if accountIdentity == nil {
		return ProviderUsage{DisplayName: "LiteLLM", Status: "untracked"}, nil
	}
	max, spent := info.BudgetMax, info.BudgetSpent
	if max == nil {
		max = info.MaxBudget
	}
	if spent == nil {
		spent = info.Spend
	}
	if max == nil || *max <= 0 || spent == nil || *spent < 0 {
		return ProviderUsage{DisplayName: "LiteLLM", Account: accountIdentity, Status: "untracked"}, nil
	}
	state := "active"
	remaining := *max - *spent
	if remaining < 0 {
		state = "exhausted"
		remaining = 0
	}
	return ProviderUsage{
		DisplayName: "LiteLLM",
		Account:     accountIdentity,
		Resources: map[string]ResourceUsage{"budget": {
			Kind: "consumption", State: state, Pool: "default", Unit: "usd",
			Limit: *max, Used: *spent, Remaining: remaining, Utilization: *spent / *max,
		}},
	}, nil
}
