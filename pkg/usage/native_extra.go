package usage

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ollama Cloud authenticates quota reads with the same-host OpenSSH Ed25519
// signing key that Ollama maintains. The key is read only for the duration of
// one request; it is never refreshed, copied, returned, or included in a
// snapshot. The request URI is signed verbatim, including the timestamp query.
type ollamaSigningKey struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	blob    []byte
}

func ollamaPoll() (ProviderUsage, error) {
	key, err := readOllamaSigningKey()
	if err != nil {
		return ProviderUsage{}, err
	}
	defer zeroOllamaKey(&key)
	return ollamaPollWithURL("https://ollama.com/api/usage", key, time.Now)
}

// The OpenCode ollama-cloud bearer credential is a distinct authority from
// both the signed ~/.ollama account and LiteLLM. The verified native endpoint
// is bounded to this exact GET; credentials are never refreshed or copied.
func ollamaCloudBearerPoll() (ProviderUsage, error) {
	credential := ollamaCloudCredential()
	if credential == "" {
		return ProviderUsage{}, pollErrf("auth-missing", "ollama-cloud bearer credential is unavailable")
	}
	return ollamaCloudBearerPollWithURL("https://ollama.com/api/usage", credential, time.Now)
}

func ollamaCloudBearerPollWithURL(endpoint, credential string, now func() time.Time) (ProviderUsage, error) {
	requestURL := strings.TrimRight(endpoint, "/") + "?ts=" + strconv.FormatInt(now().Unix(), 10)
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "ollama-cloud quota URL is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := *pollClient()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return ProviderUsage{}, netPollError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("ollama-cloud quota", resp)
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("ollama-cloud quota", resp.StatusCode)
	}
	var body ollamaUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "ollama-cloud quota decode: %v", err)
	}
	if strings.TrimSpace(body.Error) != "" {
		return ProviderUsage{}, pollErrf("provider-error", "ollama-cloud quota: %s", strings.TrimSpace(body.Error))
	}
	resources := map[string]ResourceUsage{}
	add := func(name string, usage *float64, seconds int) {
		if usage == nil || *usage < 0 || *usage > 1 {
			return
		}
		used := *usage * 100
		resources[name] = ResourceUsage{Kind: "consumption", State: "active", Pool: "default", Unit: "percent", Limit: 100, Used: used, Remaining: 100 - used, Utilization: *usage, WindowSeconds: seconds}
	}
	if body.Limits.Session != nil {
		add("session", body.Limits.Session.Usage, Window5h)
	}
	if body.Limits.Weekly != nil {
		add("weekly", body.Limits.Weekly.Usage, WindowWeekly)
	}
	if len(resources) == 0 {
		return ProviderUsage{}, pollErrf("no-windows", "ollama-cloud quota: no usable session or weekly window")
	}
	return ProviderUsage{DisplayName: "Ollama Cloud", Plan: body.Plan, Account: ollamaCloudCredentialIdentityFor(credential), Resources: resources}, nil
}

func ollamaPollWithURL(endpoint string, key ollamaSigningKey, now func() time.Time) (ProviderUsage, error) {
	ts := strconv.FormatInt(now().Unix(), 10)
	requestURI := "/api/usage?ts=" + ts
	url := strings.TrimRight(endpoint, "/")
	if parsed, err := http.NewRequest("GET", url+requestURI, nil); err == nil {
		signature := ed25519.Sign(key.private, []byte("GET,"+requestURI))
		parsed.Header.Set("Authorization", base64.StdEncoding.EncodeToString(key.blob)+":"+base64.StdEncoding.EncodeToString(signature))
		resp, requestErr := pollClient().Do(parsed)
		if requestErr != nil {
			return ProviderUsage{}, netPollError(requestErr)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return ProviderUsage{}, httpRateLimitPollError("ollama quota", resp)
		}
		if resp.StatusCode != http.StatusOK {
			return ProviderUsage{}, httpStatusPollError("ollama quota", resp.StatusCode)
		}
		var body ollamaUsageResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return ProviderUsage{}, pollErrf("decode-failed", "ollama quota decode: %v", err)
		}
		if strings.TrimSpace(body.Error) != "" {
			return ProviderUsage{}, pollErrf("provider-error", "ollama quota: %s", strings.TrimSpace(body.Error))
		}
		resources := map[string]ResourceUsage{}
		add := func(name string, usage *float64, seconds int) {
			if usage == nil || *usage < 0 || *usage > 1 {
				return
			}
			used := *usage * 100
			resources[name] = ResourceUsage{Kind: "consumption", State: "active", Pool: "default", Unit: "percent", Limit: 100, Used: used, Remaining: 100 - used, Utilization: *usage, WindowSeconds: seconds}
		}
		if body.Limits.Session != nil {
			add("session", body.Limits.Session.Usage, Window5h)
		}
		if body.Limits.Weekly != nil {
			add("weekly", body.Limits.Weekly.Usage, WindowWeekly)
		}
		if len(resources) == 0 {
			return ProviderUsage{}, pollErrf("no-windows", "ollama quota: no usable session or weekly window")
		}
		return ProviderUsage{DisplayName: "Ollama Cloud", Plan: body.Plan, Account: identity("ollama", base64.StdEncoding.EncodeToString(key.public), ollamaSignedProvenance), Resources: resources}, nil
	}
	return ProviderUsage{}, pollErrf("decode-failed", "ollama quota URL is invalid")
}

type ollamaUsageResponse struct {
	Plan   string `json:"plan"`
	Error  string `json:"error"`
	Limits struct {
		Session *struct {
			Usage *float64 `json:"usage"`
		} `json:"session"`
		Weekly *struct {
			Usage *float64 `json:"usage"`
		} `json:"weekly"`
	} `json:"limits"`
}

func ollamaKeyPath() (string, error) {
	base := strings.TrimSpace(os.Getenv("OLLAMA_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", pollErrf("auth-missing", "ollama home is unavailable")
		}
		base = filepath.Join(home, ".ollama")
	}
	return filepath.Join(base, "id_ed25519"), nil
}

func readOllamaSigningKey() (ollamaSigningKey, error) {
	path, err := ollamaKeyPath()
	if err != nil {
		return ollamaSigningKey{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ollamaSigningKey{}, pollErrf("auth-missing", "ollama signing key is unavailable; run ollama login")
	}
	defer zeroBytes(raw)
	key, parseErr := parseOllamaOpenSSHKey(raw)
	if parseErr != nil {
		return ollamaSigningKey{}, pollErrf("auth-invalid", "ollama signing key is unusable")
	}
	return key, nil
}

func parseOllamaOpenSSHKey(raw []byte) (ollamaSigningKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return ollamaSigningKey{}, fmt.Errorf("not an OpenSSH private key")
	}
	c := ollamaCursor{data: block.Bytes}
	if string(c.takeBytes(len("openssh-key-v1"))) != "openssh-key-v1" || c.takeByte() != 0 {
		return ollamaSigningKey{}, fmt.Errorf("invalid OpenSSH key magic")
	}
	if string(c.takeString()) != "none" || string(c.takeString()) != "none" || len(c.takeString()) != 0 || c.takeUint32() != 1 {
		return ollamaSigningKey{}, fmt.Errorf("encrypted or multi-key OpenSSH key")
	}
	publicBlob := append([]byte(nil), c.takeString()...)
	privateBlob := c.takeString()
	if len(publicBlob) == 0 || len(privateBlob) == 0 || c.bad {
		return ollamaSigningKey{}, fmt.Errorf("truncated OpenSSH key")
	}
	p := ollamaCursor{data: privateBlob}
	check1, check2 := p.takeUint32(), p.takeUint32()
	if p.bad || check1 != check2 || string(p.takeString()) != "ssh-ed25519" {
		return ollamaSigningKey{}, fmt.Errorf("invalid OpenSSH private section")
	}
	public := append([]byte(nil), p.takeString()...)
	private := append([]byte(nil), p.takeString()...)
	_ = p.takeString() // comment is intentionally discarded
	if p.bad || len(public) != ed25519.PublicKeySize || len(private) != ed25519.PrivateKeySize || len(private) < ed25519.SeedSize || !equalBytes(public, private[ed25519.SeedSize:]) {
		return ollamaSigningKey{}, fmt.Errorf("invalid Ed25519 key material")
	}
	publicCursor := ollamaCursor{data: publicBlob}
	if string(publicCursor.takeString()) != "ssh-ed25519" || !equalBytes(publicCursor.takeString(), public) || publicCursor.bad || len(publicCursor.data) != 0 {
		return ollamaSigningKey{}, fmt.Errorf("public key blob mismatch")
	}
	privateKey := ed25519.NewKeyFromSeed(private[:ed25519.SeedSize])
	zeroBytes(private)
	if !equalBytes(privateKey[ed25519.SeedSize:], public) {
		zeroBytes(privateKey)
		return ollamaSigningKey{}, fmt.Errorf("derived public key mismatch")
	}
	return ollamaSigningKey{private: privateKey, public: ed25519.PublicKey(public), blob: publicBlob}, nil
}

type ollamaCursor struct {
	data []byte
	bad  bool
}

func (c *ollamaCursor) takeByte() byte {
	if len(c.data) < 1 {
		c.bad = true
		return 0
	}
	b := c.data[0]
	c.data = c.data[1:]
	return b
}
func (c *ollamaCursor) takeUint32() uint32 {
	if len(c.data) < 4 {
		c.bad = true
		return 0
	}
	v := uint32(c.data[0])<<24 | uint32(c.data[1])<<16 | uint32(c.data[2])<<8 | uint32(c.data[3])
	c.data = c.data[4:]
	return v
}
func (c *ollamaCursor) takeBytes(n int) []byte {
	if n < 0 || len(c.data) < n {
		c.bad = true
		return nil
	}
	v := c.data[:n]
	c.data = c.data[n:]
	return v
}
func (c *ollamaCursor) takeString() []byte { n := c.takeUint32(); return c.takeBytes(int(n)) }
func equalBytes(a, b []byte) bool          { return len(a) == len(b) && string(a) == string(b) }
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
func zeroOllamaKey(k *ollamaSigningKey) {
	zeroBytes(k.private)
	zeroBytes(k.blob)
	k.private = nil
	k.blob = nil
	k.public = nil
}

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
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("antigravity quota", resp)
	}
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
	if key == "" {
		key = litellmConfiguredKey()
	}
	base := litellmBaseURL()
	if key == "" || base == "" {
		return ProviderUsage{}, pollErrf("auth-missing", "litellm self-key or configured base URL is unavailable")
	}
	return litellmPollWithURL(base+"/key/info", key)
}

func litellmConfiguredKey() string {
	for _, path := range opencodeAuthFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth map[string]struct {
			Key   string `json:"key"`
			Token string `json:"token"`
		}
		if json.Unmarshal(raw, &auth) != nil {
			continue
		}
		keys := make([]string, 0, len(auth))
		for name := range auth {
			if name != "opencode-go" {
				keys = append(keys, name)
			}
		}
		sort.Strings(keys)
		for _, name := range keys {
			entry := auth[name]
			if key := strings.TrimSpace(entry.Key); key != "" {
				return key
			}
			if token := strings.TrimSpace(entry.Token); token != "" {
				return token
			}
		}
	}
	return ""
}

// litellmBaseURL follows the existing OpenCode provider configuration before
// considering the explicit compatibility override. This keeps the native
// collector aligned with the CLI actually selected by the fleet.
func litellmBaseURL() string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("LITELLM_BASE_URL")), "/"); base != "" {
		return base
	}
	for _, path := range opencodeConfigFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var document any
		if json.Unmarshal(raw, &document) != nil {
			continue
		}
		if base := findProviderBaseURL(document); base != "" {
			return strings.TrimRight(base, "/")
		}
	}
	return ""
}

func findProviderBaseURL(value any) string {
	obj, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := obj[key]
		if strings.EqualFold(key, "baseURL") || strings.EqualFold(key, "base_url") || strings.EqualFold(key, "api_base") {
			if base, ok := child.(string); ok && strings.TrimSpace(base) != "" {
				return base
			}
		}
		if base := findProviderBaseURL(child); base != "" {
			return base
		}
	}
	return ""
}

func opencodeConfigFiles() []string {
	var paths []string
	if dir := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, "opencode.json"), filepath.Join(dir, "config.json"))
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		paths = append(paths, filepath.Join(dir, "opencode", "opencode.json"), filepath.Join(dir, "opencode", "config.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "opencode", "opencode.json"), filepath.Join(home, ".config", "opencode", "config.json"))
	}
	return paths
}

type opencodeUsageWindow struct {
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type opencodeUsageResponse struct {
	Rolling *opencodeUsageWindow `json:"rolling"`
	Weekly  *opencodeUsageWindow `json:"weekly"`
	Monthly *opencodeUsageWindow `json:"monthly"`
	Error   string               `json:"error"`
	Account string               `json:"account_id"`
}

func opencodePoll() (ProviderUsage, error) {
	key, err := opencodeGoKey()
	if err != nil {
		return ProviderUsage{}, err
	}
	return opencodePollWithURL("https://opencode.ai/zen/go/v1/usage", key)
}

func opencodeGoKey() (string, error) {
	for _, path := range opencodeAuthFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth map[string]struct {
			Key   string `json:"key"`
			Token string `json:"token"`
		}
		if json.Unmarshal(raw, &auth) != nil {
			return "", pollErrf("decode-failed", "opencode auth decode failed")
		}
		entry, ok := auth["opencode-go"]
		if !ok {
			continue
		}
		if key := strings.TrimSpace(entry.Key); key != "" {
			return key, nil
		}
		if token := strings.TrimSpace(entry.Token); token != "" {
			return token, nil
		}
		return "", pollErrf("auth-missing", "opencode-go credential has no usable key; run opencode login")
	}
	return "", pollErrf("auth-missing", "opencode-go credential is unavailable; run opencode login")
}

func opencodeAuthFiles() []string {
	var paths []string
	if dir := strings.TrimSpace(os.Getenv("OPENCODE_DATA_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, "auth.json"))
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		paths = append(paths, filepath.Join(dir, "opencode", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	}
	return paths
}

func opencodePollWithURL(url, token string) (ProviderUsage, error) {
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
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("opencode usage", resp)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("litellm key info", resp)
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("opencode usage", resp.StatusCode)
	}
	var body opencodeUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "opencode usage decode: %v", err)
	}
	if strings.TrimSpace(body.Error) != "" {
		return ProviderUsage{}, pollErrf("provider-error", "opencode usage: %s", strings.TrimSpace(body.Error))
	}
	resources := map[string]ResourceUsage{}
	add := func(name string, window *opencodeUsageWindow, seconds int) {
		if window == nil || window.Percent < 0 || window.Percent > 100 {
			return
		}
		resources[name] = ResourceUsage{Kind: "consumption", State: "active", Pool: "default", Unit: "percent", Limit: 100, Used: window.Percent, Remaining: 100 - window.Percent, Utilization: window.Percent / 100, ResetsAt: window.ResetsAt, WindowSeconds: seconds}
	}
	add("rolling", body.Rolling, 30*24*3600)
	add("weekly", body.Weekly, 7*24*3600)
	add("monthly", body.Monthly, 30*24*3600)
	if len(resources) == 0 {
		return ProviderUsage{}, pollErrf("no-windows", "opencode usage: no Go-plan windows")
	}
	account := identity("opencode", body.Account, "opencode-usage:account_id")
	return ProviderUsage{DisplayName: "OpenCode Go", Account: account, Resources: resources}, nil
}

func kimiPoll() (ProviderUsage, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ProviderUsage{}, pollErrf("auth-missing", "kimi home is unavailable")
	}
	config, err := os.ReadFile(filepath.Join(home, ".kimi", "config.toml"))
	if err != nil {
		return ProviderUsage{}, pollErrf("unsupported", "Kimi platform is not configured")
	}
	text := string(config)
	if !strings.Contains(strings.ToLower(text), "kimi-code") && !strings.Contains(text, "api.kimi.com/coding") {
		return ProviderUsage{}, pollErrf("unsupported", "configured Kimi platform is not Kimi Code")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kimi", "credentials", "kimi-code.json"))
	if err != nil {
		return ProviderUsage{}, pollErrf("auth-missing", "Kimi Code credentials are unavailable; run kimi /login")
	}
	var credentials struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if json.Unmarshal(raw, &credentials) != nil || strings.TrimSpace(credentials.AccessToken) == "" {
		return ProviderUsage{}, pollErrf("auth-missing", "Kimi Code credentials are unusable; run kimi /login")
	}
	base := "https://api.kimi.com/coding/v1"
	if i := strings.Index(text, "base_url"); i >= 0 {
		line := text[i:]
		if q := strings.IndexAny(line, "\"'"); q >= 0 {
			line = line[q+1:]
			if end := strings.IndexAny(line, "\"'"); end > 0 {
				base = line[:end]
			}
		}
	}
	return kimiPollWithURL(strings.TrimRight(base, "/")+"/usages", credentials.AccessToken, credentials.AccountID)
}

type kimiUsageValue struct {
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Limit     float64 `json:"limit"`
	ResetAt   string  `json:"reset_at"`
}
type kimiUsageResponse struct {
	Usage  *kimiUsageValue `json:"usage"`
	Limits []struct {
		Detail *kimiUsageValue `json:"detail"`
		Window struct {
			Duration int    `json:"duration"`
			TimeUnit string `json:"timeUnit"`
		} `json:"window"`
	} `json:"limits"`
}

func kimiPollWithURL(url, token, accountClaim string) (ProviderUsage, error) {
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
	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderUsage{}, httpRateLimitPollError("kimi usage", resp)
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, httpStatusPollError("kimi usage", resp.StatusCode)
	}
	var body kimiUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProviderUsage{}, pollErrf("decode-failed", "kimi usage decode: %v", err)
	}
	resources := map[string]ResourceUsage{}
	add := func(name string, value *kimiUsageValue, seconds int) {
		if value == nil || value.Limit <= 0 || value.Used < 0 || value.Remaining < 0 {
			return
		}
		if value.Used == 0 && value.Remaining == 0 {
			value.Used = value.Limit
		}
		resources[name] = ResourceUsage{Kind: "consumption", State: "active", Pool: "default", Unit: "requests", Limit: value.Limit, Used: value.Used, Remaining: value.Remaining, ResetsAt: value.ResetAt, WindowSeconds: seconds}
	}
	add("weekly", body.Usage, WindowWeekly)
	for i, limit := range body.Limits {
		seconds := WindowWeekly
		if limit.Window.Duration > 0 {
			seconds = limit.Window.Duration * 60
			if strings.Contains(strings.ToUpper(limit.Window.TimeUnit), "HOUR") {
				seconds = limit.Window.Duration * 3600
			}
		}
		add(fmt.Sprintf("limit%d", i+1), limit.Detail, seconds)
	}
	if len(resources) == 0 {
		return ProviderUsage{}, pollErrf("no-windows", "kimi usage: no quota windows")
	}
	return ProviderUsage{DisplayName: "Kimi Code", Account: identity("kimi", accountClaim, "kimi-code-config:account_id"), Resources: resources}, nil
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
