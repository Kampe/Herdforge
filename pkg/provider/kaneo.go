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
	"sort"
	"strings"
	"sync"
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
	// KeyTrustedOrigin is the operator-controlled origin to which APIKey may be
	// sent. It comes from KANEO_API_URL or the selected Kaneo profile, never
	// from repository-controlled APIURL.
	KeyTrustedOrigin string
	Client           *http.Client
	// Deadlines bound every op; zero fields resolve to DefaultDeadlines.
	Deadlines Deadlines
	// Retry applies to idempotent reads only (GetTask/ListTasks).
	Retry RetryPolicy
	// BulkConcurrency bounds concurrent relation fetches in ListProjectRelations.
	// Zero => DefaultBulkRelationConcurrency.
	BulkConcurrency int
	proofMu         sync.Mutex
	pendingCreates  map[string]map[string]TaskLabel

	// Receiver is the local AuthBroker over fences.db (in_progress / applied).
	// It is NOT a substitute for server-side fence+op enforcement.
	Receiver AuthoritativeReceiver
	// RequireCASMeta refuses UpdateStatus/AddComment/ClaimTask without
	// CAS meta (fence+op). Set true when attached to a ClaimStack so
	// unfenced bypass cannot skip the receiver.
	RequireCASMeta bool
	// AtomicFenceServer is true when a live FenceBroker (or hermetic enforcing
	// board under test) enforces fence+op+op-dedupe with status. Production
	// sets this only via ConfigureKaneoFenceBroker after health check — not a
	// bare env toggle. Stock Kaneo alone is never sufficient.
	AtomicFenceServer bool
	// FenceBroker is the worker-facing sidecar client (mutate + op readback).
	// Never holds mint credentials.
	FenceBroker *FenceBrokerClient
	// minter is coordinator-only (unexported). Workers never set this.
	// Per-call lease identity comes from WithMintIdentity(ctx), not mutable fields.
	minter *FenceBrokerMinter
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
		pendingCreates:   make(map[string]map[string]TaskLabel),
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
	trustedOrigin = resolveOperatorTrustedOrigin()
	if trustedOrigin == "" {
		return "", ""
	}
	return key, trustedOrigin
}

// resolveOperatorTrustedOrigin resolves the independent origin authority for
// an environment-provided key. An explicitly malformed KANEO_API_URL fails
// closed instead of falling back to another profile implicitly.
func resolveOperatorTrustedOrigin() string {
	if u := strings.TrimSpace(os.Getenv("KANEO_API_URL")); u != "" {
		if origin, err := canonicalizeHTTPOrigin(u); err == nil {
			return origin
		}
		return ""
	}
	cred := ResolveKaneoProfileCred()
	if cred.TrustedOrigin != "" {
		return cred.TrustedOrigin
	}
	return ""
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
//
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
		trusted, err := canonicalizeHTTPOrigin(k.KeyTrustedOrigin)
		if err != nil || trusted != reqOrigin {
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
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	ProjectId   string `json:"projectId"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	// Position is a pointer so JSON null/absent is distinguishable from 0
	// (board rank 0 is valid and must survive full-schema PUT rebuilds).
	Position *float64     `json:"position"`
	Labels   []kaneoLabel `json:"labels"`
}

func dtoToTask(dto kaneoTaskDTO) *Task {
	labels := make([]string, 0, len(dto.Labels))
	for _, l := range dto.Labels {
		labels = append(labels, l.Name)
	}
	createdAt, _ := time.Parse(time.RFC3339, dto.CreatedAt)
	t := &Task{
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
	if dto.UpdatedAt != "" {
		if u, err := time.Parse(time.RFC3339, dto.UpdatedAt); err == nil {
			t.UpdatedAt = u
		}
	}
	if dto.Position != nil {
		t.Position = *dto.Position
		t.HasPosition = true
	}
	return t
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
		matches := make([]kaneoTaskDTO, 0, 1)
		for _, d := range dtos {
			if kaneoTaskMatches(d, wantID) {
				matches = append(matches, d)
			}
		}
		if len(matches) > 1 {
			ids := make([]string, 0, len(matches))
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			sort.Strings(ids)
			return kaneoTaskDTO{}, fmt.Errorf("kaneo task %q is ambiguous across ids %v", wantID, ids)
		}
		if len(matches) == 1 {
			return matches[0], nil
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

// CreateTask creates a backlog card through Kaneo's authenticated API.
func (k *KaneoProvider) CreateTask(ctx context.Context, task *Task) (*Task, error) {
	if k == nil || task == nil || strings.TrimSpace(task.Title) == "" {
		return nil, fmt.Errorf("kaneo CreateTask: title is required")
	}
	projectID := strings.TrimSpace(task.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(k.ProjectID)
	}
	if projectID == "" {
		return nil, fmt.Errorf("kaneo CreateTask: project is required")
	}
	if k.UseCLI {
		return nil, fmt.Errorf("kaneo CreateTask: CLI task creation is unsupported; use the authenticated API")
	}
	ctx, cancel := WithOpDeadline(ctx, k.deadlines(), OpMutate)
	defer cancel()
	body, err := json.Marshal(map[string]interface{}{
		"title": task.Title, "description": task.Description,
		"projectId": projectID, "status": StatusToDo,
		"priority": string(task.Priority), "labels": task.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("kaneo CreateTask: marshal: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/task", strings.TrimRight(k.APIURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var dto kaneoTaskDTO
	if err := DecodeJSONResponse(resp, &dto); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider, pe.Op = "kaneo", "CreateTask"
		}
		return nil, err
	}
	if dto.ID == "" || dto.Ref == "" {
		return nil, fmt.Errorf("kaneo CreateTask: response missing task identity")
	}
	return dtoToTask(dto), nil
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

	url := fmt.Sprintf("%s/api/task/%s", k.APIURL, url.PathEscape(id))
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
	// An unfiltered read walks every column including the terminal ones. On a
	// mature board that is irreducibly slow: this board's done column alone is
	// 7 pages, and pages served concurrently slow to 14-30s each under load,
	// putting a full read around 45s. The ordinary 30s list deadline is a
	// filtered-read budget and cannot express that, so a whole-board read gets
	// the same large-board allowance relation snapshots already use. A shorter
	// caller deadline still wins, and filtered reads are unchanged.
	listCtx, cancel := WithOpDeadline(ctx, dls, OpList)
	if strings.TrimSpace(status) == "" {
		cancel()
		listCtx, cancel = context.WithTimeout(ctx, wholeBoardListDeadline(dls))
	}
	ctx = listCtx
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
		// Kaneo's unfiltered list is a board-view subset, not a complete task
		// inventory: an in-progress card can be absent despite task get resolving
		// it. Walk every canonical column when callers require the unfiltered
		// inventory used by dispatch and dependency fences.
		statuses := []string{status}
		if status == "" {
			statuses = []string{StatusToDo, StatusInProgress, StatusInReview, StatusDone, StatusPlanned, StatusArchived}
		}

		// Each status walk is an independent sequence of `kaneo` CLI spawns, so
		// walking six of them serially made the unfiltered inventory cost the SUM
		// of every column. Against a real board that exceeded the OpList deadline
		// and every caller of the unfiltered list (deps migrate, dispatch fences)
		// failed with a provider timeout. Walking columns concurrently makes the
		// cost the slowest single column instead. Pagination semantics per column
		// are unchanged: terminate only on an EMPTY page; short pages continue;
		// duplicate pages and the page cap without empty termination stay fatal.
		perStatus := make([][]kaneoTaskDTO, len(statuses))
		errs := make([]error, len(statuses))
		var wg sync.WaitGroup
		for i, listStatus := range statuses {
			wg.Add(1)
			go func(i int, listStatus string) {
				defer wg.Done()
				perStatus[i], errs[i] = k.walkStatusPages(ctx, projectID, listStatus)
			}(i, listStatus)
		}
		wg.Wait()
		// Report the first failure in status order so the error is deterministic
		// regardless of which goroutine finished first.
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		// Merge in declared status order under one accumulator; a card that moved
		// columns mid-walk must appear exactly once.
		var all []kaneoTaskDTO
		acc := NewPageAccumulator()
		for _, dtos := range perStatus {
			for _, d := range dtos {
				if acc.Add(d.ID) {
					all = append(all, d)
				}
			}
		}
		return filterTasks(all, status), nil
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

// walkStatusPages paginates one Kaneo column to exhaustion. Callers run these
// concurrently, so it must not touch shared state: dedup within the column is
// local, and cross-column dedup happens once in the caller.
func (k *KaneoProvider) walkStatusPages(ctx context.Context, projectID, listStatus string) ([]kaneoTaskDTO, error) {
	const pageSize = 100
	// The server caps a page at 100 regardless of --limit, so a large column is
	// a fixed number of round trips: this board's done column is 7 pages and
	// ~45s serially, which exceeds the list deadline on its own. Pages are
	// fetched in small concurrent batches and then CONSUMED STRICTLY IN PAGE
	// ORDER, so empty-page termination, duplicate-page detection, and the page
	// cap behave exactly as they did serially -- only the waiting overlaps.
	const batch = 4
	var out []kaneoTaskDTO
	pageAcc := NewPageAccumulator()
	for first := 1; first <= DefaultMaxListPages; first += batch {
		if err := ctx.Err(); err != nil {
			return nil, AsTimeout("kaneo", "ListTasks", OpList, k.deadlines().For(OpList), err)
		}
		last := first + batch - 1
		if last > DefaultMaxListPages {
			last = DefaultMaxListPages
		}
		n := last - first + 1
		pages := make([][]kaneoTaskDTO, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				pages[i], errs[i] = k.fetchStatusPage(ctx, projectID, listStatus, first+i, pageSize)
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			page := first + i
			// An error on a page later than the terminating one is irrelevant;
			// consuming in order means it is never reached.
			if errs[i] != nil {
				return nil, errs[i]
			}
			fresh := 0
			for _, d := range pages[i] {
				if pageAcc.Add(d.ID) {
					fresh++
					out = append(out, d)
				}
			}
			switch DecidePagination(len(pages[i]), fresh) {
			case PageStopEmpty:
				return out, nil
			case PageStopDuplicate:
				return nil, fmt.Errorf("kaneo task list (status %s, page %d): %w", listStatus, page, ErrDuplicatePage)
			}
		}
	}
	return nil, fmt.Errorf("kaneo task list (status %s): %w (maxPages=%d)", listStatus, ErrPaginationCap, DefaultMaxListPages)
}

// fetchStatusPage reads exactly one page. It holds no shared state so pages
// may be fetched concurrently; ordering decisions belong to the caller.
// kaneoListSlots bounds how many kaneo CLI reads may be in flight across the
// WHOLE process, not per column. Columns and pages are both fetched
// concurrently, so an unbounded fan-out reached 24 simultaneous CLI processes
// and the server began failing reads outright ("kaneo: exit status 1"). Six
// keeps the overlap that removes the deadline pressure while staying inside
// what the board tolerates.
var kaneoListSlots = make(chan struct{}, 6)

// wholeBoardListDeadline is the allowance for an unfiltered read. It never
// shortens a configured list deadline that is already longer.
func wholeBoardListDeadline(d Deadlines) time.Duration {
	const wholeBoardMinimum = 3 * time.Minute
	if configured := d.For(OpList); configured > wholeBoardMinimum {
		return configured
	}
	return wholeBoardMinimum
}

func acquireKaneoListSlot(ctx context.Context) error {
	select {
	case kaneoListSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseKaneoListSlot() { <-kaneoListSlots }

func (k *KaneoProvider) fetchStatusPage(ctx context.Context, projectID, listStatus string, page, pageSize int) ([]kaneoTaskDTO, error) {
	if err := acquireKaneoListSlot(ctx); err != nil {
		return nil, AsTimeout("kaneo", "ListTasks", OpList, k.deadlines().For(OpList), err)
	}
	defer releaseKaneoListSlot()

	args := []string{"task", "list", "--project", projectID, "--json",
		"--limit", fmt.Sprint(pageSize), "--page", fmt.Sprint(page), "--status", listStatus}
	res, err := kaneoRunCLI(ctx, "kaneo", args...)
	if err != nil {
		return nil, fmt.Errorf("kaneo task list (status %s, page %d): %w", listStatus, page, err)
	}
	var dtos []kaneoTaskDTO
	if err := DecodeJSONBytes(http.StatusOK, res.Stdout, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListTasks"
		}
		return nil, fmt.Errorf("kaneo task list (status %s, page %d): %w", listStatus, page, err)
	}
	return dtos, nil
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
	// Ordinary operations remain bounded by their per-operation contexts. The
	// larger transport ceiling is needed by the explicitly bounded large-board
	// relation snapshot path, which may use a two-minute graph context.
	return &http.Client{Timeout: 2*time.Minute + 5*time.Second}
}

// ListComments implements CommentReader (FAC-145): exact effect readback
// for verdict delivery. Comment bodies are returned in board order.
func (k *KaneoProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpGet)
	defer cancel()
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "comment", "list", taskID, "--json")
		if err != nil {
			return nil, fmt.Errorf("kaneo task comment list: %w", err)
		}
		var dtos []struct {
			Content string `json:"content"`
		}
		// CLI stdout is the same provider boundary as HTTP responses: a
		// successful process can still emit a structured error payload. Do not
		// let that payload become an empty/invalid verdict readback.
		if err := DecodeJSONBytes(http.StatusOK, res.Stdout, &dtos); err != nil {
			return nil, fmt.Errorf("kaneo task comment list decode: %w", err)
		}
		out := make([]string, 0, len(dtos))
		for _, d := range dtos {
			out = append(out, d.Content)
		}
		return out, nil
	}
	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	k.authorizeKaneo(req)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	var dtos []struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONResponse(resp, &dtos); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "ListComments"
		}
		return nil, err
	}
	out := make([]string, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, d.Content)
	}
	return out, nil
}
