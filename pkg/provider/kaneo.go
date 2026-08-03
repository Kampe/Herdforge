package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type KaneoProvider struct {
	APIURL    string
	ProjectID string
	UseCLI    bool
	// APIKey authenticates HTTP calls (Bearer). Loaded from api_key_env / KANEO_API_KEY.
	// Bulk project graph snapshots prefer HTTP fan-out even when UseCLI is true
	// to avoid N CLI subprocesses (FAC-159 live-path stampede).
	APIKey string
	Client *http.Client
	// Deadlines bound every op; zero fields resolve to DefaultDeadlines.
	Deadlines Deadlines
	// Retry applies to idempotent reads only (GetTask/ListTasks).
	Retry RetryPolicy
	// BulkConcurrency bounds concurrent relation fetches in ListProjectRelations.
	// Zero => DefaultBulkRelationConcurrency.
	BulkConcurrency int
}

type KaneoLinkConfig struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
}

// ResolveKaneoProjectID attempts to read project ID from .herd/kaneo.json, falling back to root .kaneo.json
func ResolveKaneoProjectID(rootDir string) string {
	paths := []string{
		filepath.Join(rootDir, ".herd", "kaneo.json"),
		filepath.Join(rootDir, ".kaneo.json"),
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			var link KaneoLinkConfig
			if err := json.Unmarshal(data, &link); err == nil && link.Project != "" {
				return link.Project
			}
		}
	}
	return ""
}

func NewKaneoProvider(apiURL string, projectID string, useCLI bool) *KaneoProvider {
	if projectID == "" {
		projectID = ResolveKaneoProjectID(".")
	}
	// Env key is origin-bound via KANEO_API_URL or profile origin — never via
	// repository-controlled apiURL argument (credential exfil defense).
	key, keyOrigin := resolveOperatorKeyAndOrigin()
	return &KaneoProvider{
		APIURL:           apiURL,
		ProjectID:        projectID,
		UseCLI:           useCLI,
		APIKey:           key,
		KeyTrustedOrigin: keyOrigin,
		Client:           kaneoHTTPClient(),
		Deadlines:        DefaultDeadlines(),
		Retry:            DefaultReadRetry(),
	}
}

// resolveOperatorKeyAndOrigin loads KANEO_API_KEY with an independent trusted
// origin: KANEO_API_URL if set, else the selected profile's api_url origin.
// Never uses repository task_provider.api_url as the trust anchor.
func resolveOperatorKeyAndOrigin() (key, trustedOrigin string) {
	key = strings.TrimSpace(os.Getenv("KANEO_API_KEY"))
	if key == "" {
		return "", ""
	}
	if u := strings.TrimSpace(os.Getenv("KANEO_API_URL")); u != "" {
		if origin, err := canonicalizeHTTPOrigin(u); err == nil {
			return key, origin
		}
		// Malformed KANEO_API_URL: key unusable for HTTP auth.
		return "", ""
	}
	// Fall back to profile origin as independent operator trust anchor.
	cred := ResolveKaneoProfileCred()
	if cred.TrustedOrigin != "" {
		return key, cred.TrustedOrigin
	}
	// Key without independent origin cannot authorize HTTP.
	return "", ""
}

// kaneoCLIAuthConfig matches the installed kaneo CLI config schema:
//
//	{ "profiles": { "default": { "api_key": "...", "api_url": "..." } },
//	  "default_profile": "default" }
//
// Never log api_key values.
type kaneoCLIAuthConfig struct {
	Profiles       map[string]kaneoCLIProfile `json:"profiles"`
	DefaultProfile string                     `json:"default_profile"`
}

type kaneoCLIProfile struct {
	APIKey      string `json:"api_key"`
	APIURL      string `json:"api_url"`
	WorkspaceID string `json:"workspace_id"`
}

// kaneoOriginCred is an origin-bound HTTP credential (key never logged).
// TrustedOrigin is scheme://host:port (canonical effective port).
type kaneoOriginCred struct {
	Key           string
	TrustedOrigin string
}

// userConfigDirFn is os.UserConfigDir; tests inject a hermetic root.
// Must return an absolute path or empty/error — never a worktree-relative dir.
var userConfigDirFn = os.UserConfigDir

// kaneoCLIConfigPath returns the canonical kaneo config.json path under
// os.UserConfigDir (macOS: ~/Library/Application Support/kaneo/config.json,
// Linux: ~/.config/kaneo/config.json). Refuses non-absolute roots so empty
// HOME/XDG cannot collapse into repo-relative credential files.
func kaneoCLIConfigPath() (string, error) {
	dir, err := userConfigDirFn()
	if err != nil {
		return "", err
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("user config dir empty")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("user config dir is not absolute (refusing worktree-relative credential path)")
	}
	return filepath.Join(dir, "kaneo", "config.json"), nil
}

// canonicalizeHTTPOrigin returns scheme://host:port for comparison.
// Rejects empty/invalid URLs, userinfo, and non-http(s) schemes.
func canonicalizeHTTPOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return canonicalizeURLOrigin(u)
}

func canonicalizeURLOrigin(u *url.URL) (string, error) {
	if u == nil {
		return "", fmt.Errorf("nil url")
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("userinfo not allowed in origin")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("empty host")
	}
	host = strings.ToLower(host)
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	// Reject non-numeric / weird ports via net.JoinHostPort validation path.
	if _, err := net.LookupPort("tcp", port); err != nil {
		// LookupPort fails for some valid numeric ports in restricted envs;
		// still require digits-only.
		for _, c := range port {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("invalid port")
			}
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// ResolveKaneoProfileCred loads the selected default_profile's api_key and
// api_url together and returns an origin-bound credential. Empty TrustedOrigin
// or Key means unusable. Never scans an arbitrary first profile; never logs the key.
func ResolveKaneoProfileCred() kaneoOriginCred {
	path, err := kaneoCLIConfigPath()
	if err != nil || path == "" || !filepath.IsAbs(path) {
		return kaneoOriginCred{}
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return kaneoOriginCred{}
	}
	var cfg kaneoCLIAuthConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return kaneoOriginCred{}
	}
	name := strings.TrimSpace(cfg.DefaultProfile)
	if name == "" || cfg.Profiles == nil {
		return kaneoOriginCred{}
	}
	prof, ok := cfg.Profiles[name]
	if !ok {
		return kaneoOriginCred{}
	}
	key := strings.TrimSpace(prof.APIKey)
	if key == "" {
		return kaneoOriginCred{}
	}
	origin, err := canonicalizeHTTPOrigin(prof.APIURL)
	if err != nil || origin == "" {
		// Key without a trusted origin is unusable for HTTP auth.
		return kaneoOriginCred{}
	}
	return kaneoOriginCred{Key: key, TrustedOrigin: origin}
}

// ResolveKaneoAPIKey is retained for tests/callers that only need a key string.
// Prefer ResolveKaneoProfileCred + origin checks for HTTP authorization.
// Order: explicit override → KANEO_API_KEY env → profile key (only when profile
// has a resolvable api_url; key alone is never returned from profile without origin).
func ResolveKaneoAPIKey(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("KANEO_API_KEY")); v != "" {
		return v
	}
	// Profile: only surface key when origin-bound pair is complete.
	cred := ResolveKaneoProfileCred()
	if cred.Key != "" && cred.TrustedOrigin != "" {
		return cred.Key
	}
	return ""
}

// authorizeKaneo attaches Bearer auth only when the request origin exactly
// matches a trusted origin for that credential:
//   - APIKey is bound to KeyTrustedOrigin (KANEO_API_URL or profile origin)
//   - profile key is bound only to canonicalize(profile.api_url)
// Repository-controlled APIURL is never the trust anchor for credentials.
func (k *KaneoProvider) authorizeKaneo(req *http.Request) {
	if k == nil || req == nil || req.URL == nil {
		return
	}
	reqOrigin, err := canonicalizeURLOrigin(req.URL)
	if err != nil {
		return
	}
	key := k.credentialForRequestOrigin(reqOrigin)
	if key == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

// credentialForRequestOrigin returns a key only if reqOrigin is trusted for it.
// Never logs keys. Never infers trust from repository-controlled APIURL.
func (k *KaneoProvider) credentialForRequestOrigin(reqOrigin string) string {
	if reqOrigin == "" {
		return ""
	}
	// 1) Operator APIKey: bound only to KeyTrustedOrigin (independent of APIURL).
	if key := strings.TrimSpace(k.APIKey); key != "" {
		trusted := strings.TrimSpace(k.KeyTrustedOrigin)
		if trusted == "" {
			return ""
		}
		// KeyTrustedOrigin may already be canonical (scheme://host:port) or a URL.
		to := trusted
		if !strings.Contains(trusted, "://") {
			// raw host not allowed — must be full origin/URL
			return ""
		}
		if o, err := canonicalizeHTTPOrigin(trusted); err == nil {
			to = o
		} else if o, err := canonicalizeHTTPOrigin("https://" + trusted); err == nil {
			// already failed; try as-is only if it looks like origin
			_ = o
			return ""
		} else {
			// If trusted is already canonical form scheme://host:port
			to = trusted
		}
		// Prefer re-canonicalize when it looks like a URL with path-less origin.
		if strings.HasPrefix(trusted, "http://") || strings.HasPrefix(trusted, "https://") {
			if o, err := canonicalizeHTTPOrigin(trusted); err == nil {
				to = o
			} else {
				return ""
			}
		}
		if to != reqOrigin {
			return ""
		}
		return key
	}
	// 2) Profile: key+api_url resolved together; exact origin match required.
	cred := ResolveKaneoProfileCred()
	if cred.Key != "" && cred.TrustedOrigin != "" && cred.TrustedOrigin == reqOrigin {
		return cred.Key
	}
	return ""
}

// credentialForAPIURL is used by graph preflight: can we authorize requests
// to this provider's configured APIURL (request origin must match trusted)?
func (k *KaneoProvider) credentialForAPIURL() string {
	if k == nil {
		return ""
	}
	origin, err := canonicalizeHTTPOrigin(k.APIURL)
	if err != nil {
		return ""
	}
	return k.credentialForRequestOrigin(origin)
}

func (k *KaneoProvider) resolvedAPIKey() string {
	// Deprecated for auth decisions — kept for tests that check "has any key material".
	// Prefer credentialForAPIURL (origin-bound).
	if k == nil {
		return ""
	}
	return k.credentialForAPIURL()
}

// kaneoHTTPClient refuses cross-origin redirects so Authorization cannot hop
// to an attacker-controlled Location after a trusted-origin first hop.
func kaneoHTTPClient() *http.Client {
	c := defaultHTTPClient()
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return http.ErrUseLastResponse
		}
		prev := via[len(via)-1]
		if prev.URL == nil || req.URL == nil {
			return fmt.Errorf("kaneo: redirect missing url")
		}
		from, err1 := canonicalizeURLOrigin(prev.URL)
		to, err2 := canonicalizeURLOrigin(req.URL)
		if err1 != nil || err2 != nil || from != to {
			// Strip any credentials that might have been copied.
			req.Header.Del("Authorization")
			return fmt.Errorf("kaneo: refusing cross-origin redirect")
		}
		// Same origin: still drop Authorization on redirect (re-auth only via authorizeKaneo on new requests).
		req.Header.Del("Authorization")
		if len(via) >= 5 {
			return fmt.Errorf("kaneo: too many redirects")
		}
		return nil
	}
	return c
}

func (k *KaneoProvider) deadlines() Deadlines {
	if k == nil {
		return DefaultDeadlines()
	}
	return k.Deadlines.Normalize()
}

func (k *KaneoProvider) readRetry() RetryPolicy {
	if k == nil {
		return DefaultReadRetry()
	}
	return k.Retry.normalize()
}

func (k *KaneoProvider) httpClient() *http.Client {
	if k != nil && k.Client != nil {
		return k.Client
	}
	return defaultHTTPClient()
}

// kaneoLabel accepts both API object form {"name":"x"} and CLI string form "x".
type kaneoLabel struct {
	Name string
}

func (l *kaneoLabel) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		l.Name = s
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	l.Name = obj.Name
	return nil
}

type kaneoTaskDTO struct {
	ID          string       `json:"id"`
	Ref         string       `json:"ref"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      string       `json:"status"`
	Priority    string       `json:"priority"`
	ProjectId   string       `json:"projectId"`
	CreatedAt   string       `json:"createdAt"`
	Labels      []kaneoLabel `json:"labels"`
}

func dtoToTask(dto kaneoTaskDTO) *Task {
	labels := make([]string, 0, len(dto.Labels))
	for _, l := range dto.Labels {
		labels = append(labels, l.Name)
	}
	createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)
	return &Task{
		ID:          dto.ID,
		Ref:         dto.Ref,
		Title:       dto.Title,
		Description: dto.Description,
		Status:      NormalizeStatus(dto.Status),
		Priority:    Priority(dto.Priority),
		ProjectID:   dto.ProjectId,
		Labels:      labels,
		CreatedAt:   createdAt,
	}
}

// kaneoTaskMatches reports whether dto is exactly the requested task by id or ref.
// Fail-closed: empty wantID never matches (callers must request a concrete id).
func kaneoTaskMatches(dto kaneoTaskDTO, wantID string) bool {
	if wantID == "" {
		return false
	}
	return dto.ID == wantID || dto.Ref == wantID
}

// decodeKaneoTaskBody accepts a single task object or a JSON array of tasks.
// Both shapes require an exact match on the requested id or ref — a sole
// nonmatching array element or an object for a different task is a hard error
// so status readback cannot confirm the wrong card.
func decodeKaneoTaskBody(statusCode int, body []byte, wantID string) (kaneoTaskDTO, error) {
	// Shared fail-closed gate for non-2xx and structured error payloads.
	if err := DecodeJSONBytes(statusCode, body, nil); err != nil {
		return kaneoTaskDTO{}, err
	}
	if wantID == "" {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task decode: requested id is required")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return kaneoTaskDTO{}, fmt.Errorf("empty task body")
	}
	if trimmed[0] == '[' {
		var dtos []kaneoTaskDTO
		if err := json.Unmarshal(trimmed, &dtos); err != nil {
			return kaneoTaskDTO{}, fmt.Errorf("decode JSON: %w", err)
		}
		if len(dtos) == 0 {
			return kaneoTaskDTO{}, fmt.Errorf("kaneo task not found: empty list")
		}
		for _, d := range dtos {
			if kaneoTaskMatches(d, wantID) {
				return d, nil
			}
		}
		// Sole nonmatching element is still a hard error (not implicit accept).
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task %q not found in list of %d", wantID, len(dtos))
	}
	var dto kaneoTaskDTO
	if err := json.Unmarshal(trimmed, &dto); err != nil {
		return kaneoTaskDTO{}, fmt.Errorf("decode JSON: %w", err)
	}
	if !kaneoTaskMatches(dto, wantID) {
		return kaneoTaskDTO{}, fmt.Errorf("kaneo task id mismatch: requested %q got id=%q ref=%q",
			wantID, dto.ID, dto.Ref)
	}
	return dto, nil
}

func (k *KaneoProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()

	var task *Task
	err := RetryRead(ctx, k.readRetry(), func(rctx context.Context) error {
		t, e := k.getTaskOnce(rctx, id)
		if e != nil {
			return AsTimeout("kaneo", "GetTask", OpGet, dls.For(OpGet), e)
		}
		task = t
		return nil
	})
	return task, err
}

// kaneoRunCLI is the CLI runner for Kaneo production UseCLI mode. Tests may
// swap it for a hermetic counter; production uses process-group RunCLI.
var kaneoRunCLI = RunCLI

func (k *KaneoProvider) getTaskOnce(ctx context.Context, id string) (*Task, error) {
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "get", id, "--json")
		if err != nil {
			return nil, fmt.Errorf("kaneo task get: %w", err)
		}
		dto, err := decodeKaneoTaskBody(http.StatusOK, res.Stdout, id)
		if err != nil {
			if pe, ok := err.(*ProviderError); ok {
				pe.Provider = "kaneo"
				pe.Op = "GetTask"
			}
			return nil, fmt.Errorf("kaneo task get: %w", err)
		}
		return dtoToTask(dto), nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	dto, err := decodeKaneoTaskBody(resp.StatusCode, body, id)
	if err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "GetTask"
		}
		return nil, err
	}
	return dtoToTask(dto), nil
}

func (k *KaneoProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	if projectID == "" {
		projectID = k.ProjectID
	}
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpList)
	defer cancel()

	var tasks []*Task
	err := RetryRead(ctx, k.readRetry(), func(rctx context.Context) error {
		t, e := k.listTasksOnce(rctx, projectID, status)
		if e != nil {
			return AsTimeout("kaneo", "ListTasks", OpList, dls.For(OpList), e)
		}
		tasks = t
		return nil
	})
	return tasks, err
}

func (k *KaneoProvider) listTasksOnce(ctx context.Context, projectID, status string) ([]*Task, error) {
	if k.UseCLI {
		// Terminate only on EMPTY page; short pages continue. Duplicate pages
		// and the page cap without empty termination are hard errors.
		// Server may cap below --limit (observed 99/100), so short-page stop
		// hides later cards (FAC-106 / board-done regressions).
		const pageSize = 100
		var all []kaneoTaskDTO
		acc := NewPageAccumulator()
		for page := 1; page <= DefaultMaxListPages; page++ {
			if err := ctx.Err(); err != nil {
				return nil, AsTimeout("kaneo", "ListTasks", OpList, k.deadlines().For(OpList), err)
			}
			args := []string{"task", "list", "--project", projectID, "--json",
				"--limit", fmt.Sprint(pageSize), "--page", fmt.Sprint(page)}
			if status != "" {
				args = append(args, "--status", status)
			}
			res, err := kaneoRunCLI(ctx, "kaneo", args...)
			if err != nil {
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			var dtos []kaneoTaskDTO
			if err := DecodeJSONBytes(http.StatusOK, res.Stdout, &dtos); err != nil {
				if pe, ok := err.(*ProviderError); ok {
					pe.Provider = "kaneo"
					pe.Op = "ListTasks"
				}
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, err)
			}
			fresh := 0
			for _, d := range dtos {
				if !acc.Add(d.ID) {
					continue
				}
				all = append(all, d)
				fresh++
			}
			dec := DecidePagination(len(dtos), fresh)
			switch dec {
			case PageStopEmpty:
				return filterTasks(all, status), nil
			case PageStopDuplicate:
				return nil, fmt.Errorf("kaneo task list (page %d): %w", page, ErrDuplicatePage)
			}
		}
		return nil, fmt.Errorf("kaneo task list: %w (maxPages=%d)", ErrPaginationCap, DefaultMaxListPages)
	}

	url := fmt.Sprintf("%s/api/task?projectId=%s", k.APIURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	var dtos []kaneoTaskDTO
	if err := DecodeJSONResponse(resp, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListTasks"
		}
		return nil, err
	}
	return filterTasks(dtos, status), nil
}

func filterTasks(dtos []kaneoTaskDTO, status string) []*Task {
	var tasks []*Task
	want := ""
	if status != "" {
		want = NormalizeStatus(status)
	}
	for _, dto := range dtos {
		t := dtoToTask(dto)
		if want != "" && t.Status != want {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	// Write the canonical lifecycle status so readback compares like-for-like
	// against dtoToTask/NormalizeStatus (production Kaneo CLI + HTTP).
	canonical := NormalizeStatus(status)
	dls := k.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := k.updateStatusOnce(writeCtx, taskID, canonical)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	// Parent ctx (not the expired write child) for readback / reconcile.
	return AfterMutation(ctx, k, dls, "kaneo", "UpdateStatus", taskID, canonical, writeErr)
}

func (k *KaneoProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "status", taskID, status, "--project", k.ProjectID)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return fmt.Errorf("kaneo task status: %s: %w", msg, err)
			}
			return fmt.Errorf("kaneo task status: %w", err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "UpdateStatus"
		}
		return err
	}
	return nil
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()

	err := k.addCommentOnce(ctx, taskID, body)
	if err == nil {
		return nil
	}
	err = AsTimeout("kaneo", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		// Comments are not status-reconcilable; never blind-retry.
		return &AmbiguousMutationError{
			Provider: "kaneo",
			Op:       "AddComment",
			TaskID:   taskID,
			WriteErr: err,
		}
	}
	return err
}

func (k *KaneoProvider) addCommentOnce(ctx context.Context, taskID, body string) error {
	if k.UseCLI {
		// Production Kaneo is multi-project; pin --project when configured
		// (matches status/list CLI paths).
		args := []string{"task", "comment", "add", taskID, body}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		res, err := kaneoRunCLI(ctx, "kaneo", args...)
		if err != nil {
			msg := cliErrMsg(res)
			if msg != "" {
				return fmt.Errorf("kaneo task comment: %s: %w", msg, err)
			}
			return fmt.Errorf("kaneo task comment: %w", err)
		}
		return nil
	}

	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "AddComment"
		}
		return err
	}
	return nil
}

func cliErrMsg(res *CLIResult) string {
	if res == nil {
		return ""
	}
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	return msg
}

// defaultHTTPClient returns a client whose transport timeout is independent of
// any single caller context — a safety net for hung bodies/connections.
// Per-op bounds still come from WithOpDeadline on the request context.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: DefaultDeadlines().Max() + 5*time.Second}
}
