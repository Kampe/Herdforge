package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Native quota pollers for claude, codex and gemini.
//
// pkg/usage polled grok natively but shelled the OpenUsage macOS helper for
// these three, so on any host without that app — a Linux box, CI — their pools
// reported nothing and the router lost proactive visibility for the surfaces it
// leans on most.
//
// The endpoints below were read out of the OpenUsage helper's own strings
// rather than guessed, and each was confirmed with a live request before this
// file was written:
//
//	claude  GET  api.anthropic.com/api/oauth/usage          -> 200, pools present
//	codex   GET  chatgpt.com/backend-api/wham/usage         -> 200, rate_limit present
//	gemini  POST cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota
//
// Every poller degrades gracefully: a missing credential, an expired token or
// an unreachable API returns an error, and the caller keeps the provider absent
// rather than reporting a fabricated zero. A zero would read as "plenty of
// quota" and send work at a surface that is actually spent.
const (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	geminiQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"

	// claudeOAuthBeta is required by the OAuth usage surface; without it the
	// endpoint rejects a token that is otherwise valid.
	claudeOAuthBeta = "oauth-2025-04-20"
	pollUserAgent   = "herdforge-usage/1.0"
	pollTimeout     = 10 * time.Second
)

func pollClient() *http.Client { return &http.Client{Timeout: pollTimeout} }

// ---------- claude ----------

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeUsage struct {
	FiveHour      *claudeWindow `json:"five_hour"`
	SevenDay      *claudeWindow `json:"seven_day"`
	SevenDayOpus  *claudeWindow `json:"seven_day_opus"`
	SevenDaySonet *claudeWindow `json:"seven_day_sonnet"`
}

// claudeToken reads the OAuth access token from the macOS keychain, where
// Claude Code stores it. There is no credentials file to read on this platform.
func claudeToken() (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return "", fmt.Errorf("claude credentials: keychain lookup failed: %w", err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			// FLOAT, not int64. The live value carries sub-millisecond
			// precision (1786223508385.367) and an int64 field fails the whole
			// decode — found by polling the real keychain, not by a fixture.
			ExpiresAt float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &creds); err != nil {
		return "", fmt.Errorf("claude credentials decode: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("claude credentials: no access token")
	}
	// expiresAt is epoch MILLISECONDS. Report expiry as expiry rather than
	// letting it surface as an opaque 401.
	if e := creds.ClaudeAiOauth.ExpiresAt; e > 0 && time.Now().After(time.UnixMilli(int64(e))) {
		return "", fmt.Errorf("claude credentials expired at %s; re-authenticate",
			time.UnixMilli(int64(e)).Format(time.RFC3339))
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

func claudePoll() (ProviderUsage, error) {
	tok, err := claudeToken()
	if err != nil {
		return ProviderUsage{}, err
	}
	return claudePollWithURL(claudeUsageURL, tok)
}

func claudePollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", pollUserAgent)

	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("claude usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, fmt.Errorf("claude usage: HTTP %d", resp.StatusCode)
	}
	var u claudeUsage
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return ProviderUsage{}, fmt.Errorf("claude usage decode: %w", err)
	}

	res := map[string]ResourceUsage{}
	// Utilization arrives as a PERCENT (0-100). ResourceUsage.Utilization is a
	// fraction elsewhere in this package (grok divides used/total), so divide —
	// mixing the two scales would make claude look 100x healthier than it is.
	add := func(name string, w *claudeWindow, window int) {
		if w == nil {
			return
		}
		res[name] = ResourceUsage{
			Kind: "consumption", Unit: "percent",
			Used: w.Utilization, Utilization: w.Utilization / 100,
			Remaining: 100 - w.Utilization, Limit: 100,
			ResetsAt: w.ResetsAt, WindowSeconds: window,
		}
	}
	add("session", u.FiveHour, 5*3600)
	add("weekly", u.SevenDay, 7*24*3600)
	add("opusWeekly", u.SevenDayOpus, 7*24*3600)
	add("sonnetWeekly", u.SevenDaySonet, 7*24*3600)
	if len(res) == 0 {
		return ProviderUsage{}, fmt.Errorf("claude usage: response carried no quota windows")
	}
	return ProviderUsage{DisplayName: "Claude", Plan: "Max", Resources: res}, nil
}

// ---------- codex ----------

type codexWindow struct {
	UsedPercent       float64 `json:"used_percent"`
	LimitWindowSecond int     `json:"limit_window_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

type codexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		Allowed      bool         `json:"allowed"`
		LimitReached bool         `json:"limit_reached"`
		Primary      *codexWindow `json:"primary_window"`
		Secondary    *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

func codexToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return "", fmt.Errorf("codex auth.json: %w", err)
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return "", fmt.Errorf("codex auth decode: %w", err)
	}
	if strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return "", fmt.Errorf("codex auth.json: no access token (OPENAI_API_KEY alone cannot read plan quota)")
	}
	return auth.Tokens.AccessToken, nil
}

func codexPoll() (ProviderUsage, error) {
	tok, err := codexToken()
	if err != nil {
		return ProviderUsage{}, err
	}
	return codexPollWithURL(codexUsageURL, tok)
}

func codexPollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", pollUserAgent)

	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("codex usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, fmt.Errorf("codex usage: HTTP %d", resp.StatusCode)
	}
	var u codexUsage
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return ProviderUsage{}, fmt.Errorf("codex usage decode: %w", err)
	}

	res := map[string]ResourceUsage{}
	add := func(name string, w *codexWindow) {
		if w == nil {
			return
		}
		r := ResourceUsage{
			Kind: "consumption", Unit: "percent",
			Used: w.UsedPercent, Utilization: w.UsedPercent / 100,
			Remaining: 100 - w.UsedPercent, Limit: 100,
			WindowSeconds: w.LimitWindowSecond,
		}
		if w.ResetAt > 0 {
			r.ResetsAt = time.Unix(w.ResetAt, 0).UTC().Format(time.RFC3339)
		}
		res[name] = r
	}
	// The primary window is the plan's long window (604800s = weekly on Pro);
	// secondary is the shorter burst window when the plan has one.
	add("weekly", u.RateLimit.Primary)
	add("session", u.RateLimit.Secondary)
	if len(res) == 0 {
		return ProviderUsage{}, fmt.Errorf("codex usage: response carried no rate-limit windows")
	}
	plan := u.PlanType
	if plan == "" {
		plan = "unknown"
	}
	return ProviderUsage{DisplayName: "Codex", Plan: plan, Resources: res}, nil
}

// ---------- gemini ----------

type geminiQuota struct {
	Quotas []struct {
		Name           string  `json:"name"`
		Limit          float64 `json:"limit"`
		Usage          float64 `json:"usage"`
		RemainingCount float64 `json:"remainingCount"`
		ResetTime      string  `json:"resetTime"`
	} `json:"quotas"`
}

func geminiToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "oauth_creds.json"))
	if err != nil {
		return "", fmt.Errorf("gemini oauth_creds.json: %w", err)
	}
	var creds struct {
		AccessToken string `json:"access_token"`
		ExpiryDate  int64  `json:"expiry_date"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("gemini creds decode: %w", err)
	}
	if creds.AccessToken == "" {
		return "", fmt.Errorf("gemini oauth_creds.json: no access token")
	}
	// expiry_date is epoch MILLISECONDS. Saying "expired, re-authenticate" is
	// actionable; a bare 401 from the API is not.
	if creds.ExpiryDate > 0 && time.Now().After(time.UnixMilli(creds.ExpiryDate)) {
		return "", fmt.Errorf("gemini credentials expired at %s; run the gemini CLI to refresh",
			time.UnixMilli(creds.ExpiryDate).Format(time.RFC3339))
	}
	return creds.AccessToken, nil
}

func geminiPoll() (ProviderUsage, error) {
	tok, err := geminiToken()
	if err != nil {
		return ProviderUsage{}, err
	}
	return geminiPollWithURL(geminiQuotaURL, tok)
}

func geminiPollWithURL(url, token string) (ProviderUsage, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader("{}"))
	if err != nil {
		return ProviderUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", pollUserAgent)

	resp, err := pollClient().Do(req)
	if err != nil {
		return ProviderUsage{}, fmt.Errorf("gemini quota: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, fmt.Errorf("gemini quota: HTTP %d", resp.StatusCode)
	}
	var q geminiQuota
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return ProviderUsage{}, fmt.Errorf("gemini quota decode: %w", err)
	}

	res := map[string]ResourceUsage{}
	for _, item := range q.Quotas {
		if item.Name == "" {
			continue
		}
		util := 0.0
		if item.Limit > 0 {
			util = item.Usage / item.Limit
		}
		res[item.Name] = ResourceUsage{
			Kind: "consumption", Unit: "requests",
			Limit: item.Limit, Used: item.Usage,
			Remaining: item.RemainingCount, Utilization: util,
			ResetsAt: item.ResetTime,
		}
	}
	if len(res) == 0 {
		return ProviderUsage{}, fmt.Errorf("gemini quota: response carried no quotas")
	}
	return ProviderUsage{DisplayName: "Gemini", Plan: "Pro", Resources: res}, nil
}
