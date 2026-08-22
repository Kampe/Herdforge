package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// FenceBrokerClient is the worker-facing broker client.
//
// It holds ONLY the worker mutate credential. Per-op capabilities are
// passed immutably to MutateStatus (never stored on the shared client —
// concurrent tasks must not cross-wire grants).
type FenceBrokerClient struct {
	BaseURL    string // http://127.0.0.1:port or http://unix
	Token      string // worker token only (never mint)
	UnixSocket string
	Client     *http.Client
}

// CapabilityIssueRequest is the lease-bound mint request (minter only).
type CapabilityIssueRequest struct {
	BoardTaskID string
	TaskID      string
	TaskRef     string
	Repo        string
	Provider    string
	Project     string
	OwnerID     string
	Generation  int64
	OpID        string
	Action      string // status | comment
	Status      string
	Comment     string
}

// NewFenceBrokerClientFromEnv builds a worker client (no mint authority).
// HERD_FENCE_BROKER_MINT_TOKEN is intentionally ignored if present.
func NewFenceBrokerClientFromEnv() (*FenceBrokerClient, error) {
	url := strings.TrimSpace(os.Getenv(envFenceBrokerURL))
	tok := strings.TrimSpace(os.Getenv(envFenceBrokerToken))
	if url == "" {
		return nil, fmt.Errorf("provider: %s not set", envFenceBrokerURL)
	}
	if tok == "" || len(tok) < 16 {
		return nil, fmt.Errorf("provider: %s required (min 16)", envFenceBrokerToken)
	}
	c := &FenceBrokerClient{Token: tok}
	if err := c.bindURL(url); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *FenceBrokerClient) bindURL(url string) error {
	if strings.HasPrefix(url, "unix://") {
		c.UnixSocket = strings.TrimPrefix(url, "unix://")
		c.BaseURL = unixBaseURL
		return nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		if strings.HasPrefix(url, "http://") {
			rest := strings.TrimPrefix(url, "http://")
			host := rest
			if i := strings.Index(rest, "/"); i >= 0 {
				host = rest[:i]
			}
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			ip := net.ParseIP(host)
			if host != "localhost" && host != "unix" && (ip == nil || !ip.IsLoopback()) {
				return fmt.Errorf("provider: refusing non-loopback cleartext broker URL %q", url)
			}
		}
		c.BaseURL = strings.TrimRight(url, "/")
		return nil
	}
	return fmt.Errorf("provider: bad broker URL scheme (use http://127.0.0.1:… or unix:///path)")
}

func (c *FenceBrokerClient) httpClient() *http.Client {
	if c != nil && c.Client != nil {
		return c.Client
	}
	if c != nil && c.UnixSocket != "" {
		return &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", c.UnixSocket)
				},
			},
		}
	}
	return defaultHTTPClient()
}

func (c *FenceBrokerClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(brokerAuthHeader, c.Token)
	return c.httpClient().Do(req)
}

// Live reports whether the broker is reachable (liveness).
func (c *FenceBrokerClient) Live(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("nil fence broker client")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return fmt.Errorf("fence-broker health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("fence-broker health HTTP %d", resp.StatusCode)
	}
	return nil
}

// OpApplied is server-native readback (worker-safe).
func (c *FenceBrokerClient) OpApplied(ctx context.Context, opID, taskID, wantStatus string) (bool, error) {
	if c == nil || opID == "" {
		return false, fmt.Errorf("fence-broker: op lookup requires client+op")
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/ops/"+opID, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err := rejectJSONErrorBody(resp.StatusCode, body); err != nil {
		return false, err
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("fence-broker op lookup HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Applied        bool   `json:"applied"`
		Ambiguous      bool   `json:"ambiguous"`
		TaskID         string `json:"task_id"`
		ExpectedStatus string `json:"expected_status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, err
	}
	if !out.Applied || out.Ambiguous {
		return false, nil
	}
	if out.TaskID != taskID {
		return false, nil
	}
	if wantStatus != "" && NormalizeStatus(out.ExpectedStatus) != NormalizeStatus(wantStatus) {
		return false, nil
	}
	return true, nil
}

// MutateStatus performs broker-enforced status mutation with an immutable
// per-op pre-minted capability. Never mints. Never stores capability on client.
func (c *FenceBrokerClient) MutateStatus(ctx context.Context, taskID, status string, fence int64, opID, capability string) error {
	if c == nil {
		return fmt.Errorf("nil fence broker client")
	}
	if taskID == "" || status == "" || opID == "" || fence <= 0 {
		return fmt.Errorf("fence-broker mutate: task, status, fence, op required")
	}
	if capability == "" {
		return fmt.Errorf("fence-broker mutate: pre-minted capability required (worker clients cannot mint)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL+"/v1/tasks/"+taskID+"/status",
		bytes.NewReader(mustJSON(map[string]string{"status": NormalizeStatus(status)})))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(brokerAuthHeader, c.Token)
	req.Header.Set("X-Herd-Op", opID)
	req.Header.Set("X-Herd-Fence", fmt.Sprintf("%d", fence))
	req.Header.Set(mutationCapabilityHeader, capability)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := rejectJSONErrorBody(resp.StatusCode, body); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: fence-broker rejected fence/op: %s", claim.ErrProviderFenceRejected, body)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: fence-broker authz: %s", claim.ErrProviderFenceRejected, body)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fence-broker mutate HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// MutateComment performs broker-enforced comment mutation with per-op capability.
func (c *FenceBrokerClient) MutateComment(ctx context.Context, taskID, commentBody string, fence int64, opID, capability string) error {
	if c == nil {
		return fmt.Errorf("nil fence broker client")
	}
	if taskID == "" || commentBody == "" || opID == "" || fence <= 0 || capability == "" {
		return fmt.Errorf("fence-broker comment: task, body, fence, op, capability required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/tasks/"+taskID+"/comment",
		bytes.NewReader(mustJSON(map[string]string{"body": commentBody})))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(brokerAuthHeader, c.Token)
	req.Header.Set("X-Herd-Op", opID)
	req.Header.Set("X-Herd-Fence", fmt.Sprintf("%d", fence))
	req.Header.Set(mutationCapabilityHeader, capability)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := rejectJSONErrorBody(resp.StatusCode, body); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: fence-broker comment rejected: %s", claim.ErrProviderFenceRejected, body)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fence-broker comment HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// rejectJSONErrorBody enforces fail-closed: HTTP 2xx with {"error":...} is a hard error.
func rejectJSONErrorBody(status int, body []byte) error {
	if status < 200 || status >= 300 || len(body) == 0 {
		return nil
	}
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	if strings.TrimSpace(probe.Error) != "" {
		return fmt.Errorf("fence-broker: HTTP %d body carries error (fail-closed): %s", status, probe.Error)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ConfigureKaneoFenceBroker attaches a worker FenceBroker client after live check.
// Refuses any client that appears to carry mint authority via type confusion.
func ConfigureKaneoFenceBroker(k *KaneoProvider, client *FenceBrokerClient) error {
	if k == nil {
		return fmt.Errorf("nil KaneoProvider")
	}
	if client == nil {
		return fmt.Errorf("nil FenceBrokerClient")
	}
	if err := client.Live(context.Background()); err != nil {
		return fmt.Errorf("fence-broker not live: %w", err)
	}
	k.FenceBroker = client
	k.AtomicFenceServer = true
	return nil
}

// NewFenceBrokerClientForTest builds a worker client for a running broker.
// Uses unexported broker fields only (no exported secret getters).
func NewFenceBrokerClientForTest(b *FenceBroker) *FenceBrokerClient {
	if b == nil {
		return nil
	}
	c := &FenceBrokerClient{Token: b.token}
	if sock := b.UnixSocket(); sock != "" {
		c.UnixSocket = sock
		c.BaseURL = unixBaseURL
	} else {
		c.BaseURL = b.ClientBaseURL()
	}
	return c
}
