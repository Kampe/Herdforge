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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FenceBrokerMinter is the coordinator-only mint channel.
//
// The mint secret is unexported and never returned by methods, String(),
// or JSON. Construction requires the claim-dir credential file written by
// StartFenceBroker (mode 0600) — not HERD_FENCE_BROKER_MINT_TOKEN in env.
// Workers must not receive that file path; env mint alone always fails.
type FenceBrokerMinter struct {
	baseURL    string
	unixSocket string
	mintSecret string // unexported — never expose
	httpClient *http.Client
	claimDir   string
}

const (
	envFenceCoordinator = "HERD_FENCE_COORDINATOR"
	envFenceMintCred    = "HERD_FENCE_MINT_CRED_FILE" // absolute path under claim dir only
)

// NewFenceBrokerMinterFromEnv is intentionally disabled for induction resistance.
// Mint secret must not be loadable from process environment alone.
func NewFenceBrokerMinterFromEnv() (*FenceBrokerMinter, error) {
	return nil, fmt.Errorf("provider: env mint disabled (forgeable); use NewFenceBrokerMinterFromClaimDir or coordinator mint-cred file")
}

// ScrubWorkerMintEnv removes mint material from the process environment so
// workers cannot inherit launcher secrets. Safe to call on every worker start.
func ScrubWorkerMintEnv() {
	_ = os.Unsetenv(envFenceBrokerMintToken)
	_ = os.Unsetenv(envFenceCoordinator)
	_ = os.Unsetenv(envFenceMintCred)
}

// NewFenceBrokerMinterFromClaimDir is BLOCKED outside hermetic tests: a mode-0600
// file in a shared claim dir is readable by any same-UID worker process and is
// not a non-forgeable OS boundary. Production mint authority is deferred to
// FAC-169 (process/UID/FD boundary). Tests may use this under testing.Testing()
// or prefer newMinterForTest (in-process unexported secret).
func NewFenceBrokerMinterFromClaimDir(claimDir, brokerURL string) (*FenceBrokerMinter, error) {
	if !testing.Testing() {
		return nil, fmt.Errorf("provider: claim-dir mint BLOCKED pending FAC-169 (same-UID fence-mint.cred is not authority; do not claim completion)")
	}
	if strings.TrimSpace(claimDir) == "" {
		return nil, fmt.Errorf("provider: minter claimDir required")
	}
	abs, err := filepath.Abs(claimDir)
	if err != nil {
		return nil, err
	}
	credPath := filepath.Join(abs, mintCredLeaf)
	if override := strings.TrimSpace(os.Getenv(envFenceMintCred)); override != "" {
		oabs, err := filepath.Abs(override)
		if err != nil {
			return nil, err
		}
		// Must stay under claim dir (non-copyable-ish launcher path).
		rel, err := filepath.Rel(abs, oabs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("provider: mint cred file must be under claim dir")
		}
		credPath = oabs
	}
	secret, err := readMintCredentialFile(credPath)
	if err != nil {
		return nil, err
	}
	// Refuse if env still carries mint secret (worker leakage) — scrub first.
	if envTok := strings.TrimSpace(os.Getenv(envFenceBrokerMintToken)); envTok != "" && envTok == secret {
		return nil, fmt.Errorf("provider: mint secret present in env (worker leakage); call ScrubWorkerMintEnv / refuse shared env")
	}
	if err := refuseEnvMintLeak(); err != nil {
		return nil, err
	}
	brokerURL, err = resolveMinterBrokerURL(brokerURL)
	if err != nil {
		return nil, err
	}
	m := &FenceBrokerMinter{mintSecret: secret, claimDir: abs}
	if err := m.bindURL(brokerURL); err != nil {
		return nil, err
	}
	return m, nil
}

// refuseEnvMintLeak refuses whenever mint material is present in the process
// environment. One definition, because both constructors enforce it and a copy
// that drifted would silently accept an env-forged authority.
func refuseEnvMintLeak() error {
	if strings.TrimSpace(os.Getenv(envFenceBrokerMintToken)) != "" {
		return fmt.Errorf("provider: refuse minter while %s is set in environment (scrub worker env)", envFenceBrokerMintToken)
	}
	return nil
}

// resolveMinterBrokerURL applies the single rule for where a minter's broker
// endpoint comes from: the explicit argument, else the environment, else refuse.
func resolveMinterBrokerURL(explicit string) (string, error) {
	if url := strings.TrimSpace(explicit); url != "" {
		return url, nil
	}
	if url := strings.TrimSpace(os.Getenv(envFenceBrokerURL)); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("provider: broker URL required for minter")
}

func readMintCredentialFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("provider: mint credential file: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("provider: refuse symlink mint credential")
	}
	// Require owner-only bits (unix): reject world/group readable.
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("provider: mint credential must be mode 0600, got %o", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(raw))
	if len(secret) < 16 {
		return "", fmt.Errorf("provider: mint credential too short")
	}
	return secret, nil
}

func (m *FenceBrokerMinter) bindURL(url string) error {
	if strings.HasPrefix(url, "unix://") {
		m.unixSocket = strings.TrimPrefix(url, "unix://")
		m.baseURL = unixBaseURL
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
				return fmt.Errorf("provider: refusing non-loopback cleartext mint URL %q", url)
			}
		}
		m.baseURL = strings.TrimRight(url, "/")
		return nil
	}
	return fmt.Errorf("provider: bad mint broker URL scheme")
}

func (m *FenceBrokerMinter) client() *http.Client {
	if m != nil && m.httpClient != nil {
		return m.httpClient
	}
	if m != nil && m.unixSocket != "" {
		return &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", m.unixSocket)
				},
			},
		}
	}
	return defaultHTTPClient()
}

// IssueCapability mints one single-use op-bound capability JSON for a worker.
// Returns only the capability document — never the mint secret.
func (m *FenceBrokerMinter) IssueCapability(ctx context.Context, req CapabilityIssueRequest) (string, error) {
	if m == nil || m.mintSecret == "" {
		return "", fmt.Errorf("fence-broker minter: not configured")
	}
	boardID := req.BoardTaskID
	if boardID == "" {
		boardID = req.TaskID
	}
	if req.TaskRef == "" {
		req.TaskRef = boardID
	}
	if boardID == "" {
		boardID = req.TaskRef
	}
	if req.Repo == "" || req.Provider == "" || req.Project == "" || req.TaskRef == "" {
		return "", fmt.Errorf("fence-broker minter: full LeaseKey required")
	}
	action := req.Action
	if action == "" {
		action = capActionStatus
	}
	if boardID == "" || req.OwnerID == "" || req.OpID == "" || req.Generation <= 0 {
		return "", fmt.Errorf("fence-broker minter: board task, owner, op, generation required")
	}
	if action == capActionStatus && req.Status == "" {
		return "", fmt.Errorf("fence-broker minter: status required")
	}
	if action == capActionComment && req.Comment == "" {
		return "", fmt.Errorf("fence-broker minter: comment required")
	}
	body, err := json.Marshal(map[string]any{
		"board_task_id": boardID, "task_id": boardID, "task_ref": req.TaskRef,
		"repo": req.Repo, "provider": req.Provider, "project": req.Project,
		"owner_id": req.OwnerID, "generation": req.Generation,
		"op_id": req.OpID, "action": action,
		"status": NormalizeStatus(req.Status), "comment": req.Comment,
	})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/v1/capabilities", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(mintAuthHeader, m.mintSecret)
	resp, err := m.client().Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if err := rejectJSONErrorBody(resp.StatusCode, b); err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fence-broker mint HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	if out.Capability == "" {
		return "", fmt.Errorf("fence-broker mint: empty capability")
	}
	return out.Capability, nil
}

// String redacts secrets (safe for logs).
func (m *FenceBrokerMinter) String() string {
	if m == nil {
		return "FenceBrokerMinter<nil>"
	}
	return fmt.Sprintf("FenceBrokerMinter{baseURL:%q,mint:redacted}", m.baseURL)
}

// MarshalJSON never serializes the mint secret.
func (m *FenceBrokerMinter) MarshalJSON() ([]byte, error) {
	return []byte(`{"role":"fence-broker-minter","mint":"redacted"}`), nil
}

// newMinterForTest constructs a minter from an in-process broker (same package).
func newMinterForTest(b *FenceBroker) *FenceBrokerMinter {
	if b == nil {
		return nil
	}
	m := &FenceBrokerMinter{mintSecret: b.mintToken, claimDir: b.claimDir}
	if sock := b.UnixSocket(); sock != "" {
		m.unixSocket = sock
		m.baseURL = unixBaseURL
	} else {
		m.baseURL = b.ClientBaseURL()
	}
	return m
}

// AttachCoordinatorMinter grants a KaneoProvider the ability to mint
// capabilities at mutate time (coordinator processes only).
func AttachCoordinatorMinter(k *KaneoProvider, m *FenceBrokerMinter) error {
	if k == nil {
		return fmt.Errorf("nil KaneoProvider")
	}
	if m == nil || m.mintSecret == "" {
		return fmt.Errorf("nil or empty minter")
	}
	k.minter = m
	return nil
}

// ClearCoordinatorMinter removes mint authority (tests / worker hardening).
func ClearCoordinatorMinter(k *KaneoProvider) {
	if k != nil {
		k.minter = nil
	}
}
