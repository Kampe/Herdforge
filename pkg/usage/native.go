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

	claudeCredentialFile  = ".credentials.json"
	claudeKeychainService = "Claude Code-credentials"
	pollTimeout           = 10 * time.Second
)

func pollClient() *http.Client { return &http.Client{Timeout: pollTimeout} }

// ---------- claude ----------

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeScopedLimit struct {
	Kind     string  `json:"kind"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeUsage struct {
	FiveHour      *claudeWindow `json:"five_hour"`
	SevenDay      *claudeWindow `json:"seven_day"`
	SevenDaySonet *claudeWindow `json:"seven_day_sonnet"`
	// Limits carries PER-MODEL weekly pools that appear nowhere else in the
	// response — Fable is only here. Reading upstream OpenUsage's mapper is
	// what surfaced this; inferring endpoints from the binary's strings got the
	// URL right and the response shape wrong.
	Limits []claudeScopedLimit `json:"limits"`
}

// claudeToken resolves the OAuth access token the way Claude Code itself does.
//
// The first implementation read ONLY the macOS keychain, which cannot work on
// Linux — the portability this package exists for. Upstream OpenUsage resolves
// a credentials FILE first and treats the keychain as one source among several,
// honouring CLAUDE_CONFIG_DIR and XDG_CONFIG_HOME. Ported here in that order,
// so a Linux host with ~/.claude/.credentials.json works with no keychain at all.
func claudeToken() (string, error) {
	for _, path := range claudeCredentialFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		tok, err := claudeTokenFromJSON(raw)
		if err == nil {
			return tok, nil
		}
		// A present-but-expired file is a definitive answer, not a reason to
		// keep looking: silently falling through to another login would report
		// a different account's quota.
		if strings.Contains(err.Error(), "expired") {
			return "", err
		}
	}
	out, err := exec.Command("security", "find-generic-password", "-s", claudeKeychainService, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("claude credentials: no credentials file and keychain lookup failed")
	}
	return claudeTokenFromJSON(bytes.TrimSpace(out))
}

// claudeCredentialFiles lists candidate credential files, most specific first.
func claudeCredentialFiles() []string {
	var out []string
	add := func(dir string) {
		if strings.TrimSpace(dir) != "" {
			out = append(out, filepath.Join(dir, claudeCredentialFile))
		}
	}
	add(os.Getenv("CLAUDE_CONFIG_DIR"))
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		add(filepath.Join(x, "claude"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude"))
		add(filepath.Join(home, ".config", "claude"))
	}
	return out
}

func claudeTokenFromJSON(raw []byte) (string, error) {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			// FLOAT, not int64. The live value carries sub-millisecond
			// precision (1786223508385.367) and an int64 field fails the whole
			// decode — found by polling real credentials, not by a fixture.
			ExpiresAt float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("claude credentials decode: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("claude credentials: no access token")
	}
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
	add("sonnetWeekly", u.SevenDaySonet, 7*24*3600)
	// weekly_scoped entries are separate quota pools the router ranks on:
	// herdr-quota tracks claude/fable independently of claude/default, and a
	// spent default pool does not imply a spent Fable pool.
	for _, l := range u.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))
		if name == "" {
			continue
		}
		res[name+"Weekly"] = ResourceUsage{
			Kind: "consumption", Unit: "percent",
			Used: l.Percent, Utilization: l.Percent / 100,
			Remaining: 100 - l.Percent, Limit: 100,
			ResetsAt: l.ResetsAt, WindowSeconds: 7 * 24 * 3600,
		}
	}
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

type codexRateLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}

type codexUsage struct {
	PlanType  string         `json:"plan_type"`
	RateLimit codexRateLimit `json:"rate_limit"`
	// AdditionalRateLimits carries per-model pools — Spark lives ONLY here.
	// Missing it reported codex as 95% spent while Spark had 87% free, which
	// would make the router skip the one healthy surface it had.
	AdditionalRateLimits []struct {
		LimitName string         `json:"limit_name"`
		RateLimit codexRateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
}

// codexToken resolves the Codex OAuth token from the homes the CLI uses:
// $CODEX_HOME, then ~/.config/codex, then ~/.codex. The first implementation
// checked only the last, so a CODEX_HOME or XDG-style install reported no quota.
func codexToken() (string, error) {
	var lastErr error
	for _, path := range codexCredentialFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(raw, &auth); err != nil {
			lastErr = fmt.Errorf("codex auth decode: %w", err)
			continue
		}
		if strings.TrimSpace(auth.Tokens.AccessToken) != "" {
			return auth.Tokens.AccessToken, nil
		}
		// An API-key-only auth.json cannot read plan quota; upstream refuses it
		// for the same reason rather than sending a request that will 401.
		lastErr = fmt.Errorf("codex auth.json at %s has no OAuth access token (an API key alone cannot read plan quota)", path)
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("codex auth.json: not found in $CODEX_HOME, ~/.config/codex or ~/.codex")
}

func codexCredentialFiles() []string {
	var out []string
	if h := strings.TrimSpace(os.Getenv("CODEX_HOME")); h != "" {
		out = append(out, filepath.Join(h, "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".config", "codex", "auth.json"),
			filepath.Join(home, ".codex", "auth.json"))
	}
	return out
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
	for _, extra := range u.AdditionalRateLimits {
		key := codexPoolKey(extra.LimitName)
		if key == "" {
			continue
		}
		add(key+"Weekly", extra.RateLimit.Primary)
		add(key+"Session", extra.RateLimit.Secondary)
	}
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

// geminiToken resolves the Gemini CLI's OAuth token, honouring GEMINI_CONFIG_DIR
// and XDG_CONFIG_HOME rather than assuming ~/.gemini.
func geminiToken() (string, error) {
	var lastErr error
	for _, path := range geminiCredentialFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var creds struct {
			AccessToken string  `json:"access_token"`
			ExpiryDate  float64 `json:"expiry_date"`
		}
		if err := json.Unmarshal(raw, &creds); err != nil {
			lastErr = fmt.Errorf("gemini creds decode: %w", err)
			continue
		}
		if creds.AccessToken == "" {
			lastErr = fmt.Errorf("gemini creds at %s: no access token", path)
			continue
		}
		// expiry_date is epoch MILLISECONDS. "Expired, re-authenticate" is
		// actionable; a bare 401 from the API is not. Float for the same reason
		// claude's expiresAt is — do not assume an integer.
		if e := creds.ExpiryDate; e > 0 && time.Now().After(time.UnixMilli(int64(e))) {
			return "", fmt.Errorf("gemini credentials expired at %s; run the gemini CLI to refresh",
				time.UnixMilli(int64(e)).Format(time.RFC3339))
		}
		return creds.AccessToken, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("gemini oauth_creds.json: not found in $GEMINI_CONFIG_DIR, $XDG_CONFIG_HOME/gemini or ~/.gemini")
}

func geminiCredentialFiles() []string {
	var out []string
	add := func(dir string) {
		if strings.TrimSpace(dir) != "" {
			out = append(out, filepath.Join(dir, "oauth_creds.json"))
		}
	}
	add(os.Getenv("GEMINI_CONFIG_DIR"))
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		add(filepath.Join(x, "gemini"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".gemini"))
		add(filepath.Join(home, ".config", "gemini"))
	}
	return out
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

// codexPoolKey turns a per-model limit name into a pool key.
// "GPT-5.3-Codex-Spark" -> "spark", matching the pool herdr-quota reports.
func codexPoolKey(limitName string) string {
	n := strings.ToLower(strings.TrimSpace(limitName))
	if n == "" {
		return ""
	}
	if i := strings.LastIndex(n, "-"); i >= 0 && i+1 < len(n) {
		return n[i+1:]
	}
	return n
}
