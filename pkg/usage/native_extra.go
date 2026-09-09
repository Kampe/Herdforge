package usage

import (
	"encoding/json"
	"net/http"
	"strings"
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
	return ProviderUsage{}, pollErrf("unsupported", "antigravity language-server discovery unavailable; AGY must already expose a same-host quota service")
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
	BudgetMax   *float64 `json:"budget_max"`
	BudgetSpent *float64 `json:"budget_spent"`
	MaxBudget   *float64 `json:"max_budget"`
	Spend       *float64 `json:"spend"`
	RPMLimit    *float64 `json:"rpm_limit"`
	MaxParallel *float64 `json:"max_parallel_requests"`
}

func litellmPoll() (ProviderUsage, error) {
	return ProviderUsage{}, pollErrf("auth-missing", "litellm self-key is unavailable; run the owning CLI login")
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
	max, spent := info.BudgetMax, info.BudgetSpent
	if max == nil {
		max = info.MaxBudget
	}
	if spent == nil {
		spent = info.Spend
	}
	if max == nil || *max <= 0 || spent == nil || *spent < 0 || *spent > *max {
		return ProviderUsage{DisplayName: "LiteLLM", Status: "untracked"}, nil
	}
	return ProviderUsage{DisplayName: "LiteLLM", Resources: map[string]ResourceUsage{"budget": {Kind: "consumption", State: "active", Pool: "default", Unit: "percent", Limit: *max, Used: *spent, Remaining: *max - *spent, Utilization: *spent / *max}}}, nil
}
