package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/security"
)

// Shareable worker-broker errors. Standing board-write launch must refuse
// these before a tab exists.
var (
	ErrWorkerBrokerMissing   = errors.New("standing board-write requires a shareable standalone fence broker (herd fence-broker --claim-dir)")
	ErrWorkerBrokerPartial   = errors.New("standing board-write: HERD_FENCE_BROKER_URL and HERD_FENCE_BROKER_TOKEN must both be set")
	ErrWorkerBrokerUnhealthy = errors.New("standing board-write: fence broker is unhealthy")
	ErrWorkerBrokerForeign   = errors.New("standing board-write: fence broker claim volume does not match this repository")
	ErrWorkerBrokerInProcess = errors.New("standing board-write: in-process coordinator ownership has no shareable worker route; run a coordinator-managed standalone broker: herd fence-broker --claim-dir")
	ErrWorkerBrokerMint      = errors.New("standing board-write: mint/coordinator authority cannot be forwarded as a worker credential")
)

// ShareableWorkerBroker is a validated standalone worker capability.
// Token is unexported from JSON/String; mint material is never stored.
type ShareableWorkerBroker struct {
	URL      string
	Token    string
	ClaimDir string
}

// MarshalJSON redacts the worker token (safe for receipts/logs).
func (s ShareableWorkerBroker) MarshalJSON() ([]byte, error) {
	url := ""
	claim := ""
	if s.URL != "" {
		url = s.URL
	}
	if s.ClaimDir != "" {
		claim = s.ClaimDir
	}
	return json.Marshal(struct {
		URL      string `json:"url,omitempty"`
		Token    string `json:"token,omitempty"`
		ClaimDir string `json:"claim_dir,omitempty"`
	}{URL: url, Token: "redacted", ClaimDir: claim})
}

func (s ShareableWorkerBroker) String() string {
	return fmt.Sprintf("ShareableWorkerBroker{url:%q,token:redacted,claim_dir:%q}", s.URL, s.ClaimDir)
}

// ConfidentialWorkerBrokerEnvFile is the existing contain env.list channel
// (mode 0600, gitignored). Worker credentials travel here, never herdr --env argv.
func ConfidentialWorkerBrokerEnvFile(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".herd", "contain", "env.list")
}

// ResolveShareableWorkerBroker validates the existing standalone claim-volume
// broker as a worker-only launch capability. It never starts a broker, never
// mints, and never exports coordinator ownership.
func ResolveShareableWorkerBroker(ctx context.Context, claimDir string) (*ShareableWorkerBroker, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	url := strings.TrimSpace(os.Getenv(envFenceBrokerURL))
	tok := strings.TrimSpace(os.Getenv(envFenceBrokerToken))
	mint := strings.TrimSpace(os.Getenv(envFenceBrokerMintToken))

	if coordinatorOwnsBroker() {
		return nil, ErrWorkerBrokerInProcess
	}
	if url == "" && tok == "" {
		return nil, ErrWorkerBrokerMissing
	}
	if url == "" || tok == "" || len(tok) < 16 {
		return nil, ErrWorkerBrokerPartial
	}
	if mint != "" && mint == tok {
		return nil, fmt.Errorf("%w: worker token equals mint token", ErrWorkerBrokerMint)
	}

	client, err := NewFenceBrokerClientFromEnv()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerPartial, err)
	}
	if err := client.Live(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerUnhealthy, err)
	}
	st, err := client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerPartial, err)
	}

	wantDir := strings.TrimSpace(claimDir)
	if wantDir == "" {
		dir, derr := CanonicalClaimDir(".", firstNonEmpty(os.Getenv("HERD_ROOT"), os.Getenv("HERD_REPO_ROOT")))
		if derr != nil {
			return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerForeign, derr)
		}
		wantDir = dir
	}
	wantAbs, err := filepath.Abs(wantDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerForeign, err)
	}
	gotAbs, err := filepath.Abs(strings.TrimSpace(st.ClaimDir))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerForeign, err)
	}
	if !sameClaimPath(wantAbs, gotAbs) {
		return nil, fmt.Errorf("%w: broker claim_dir %q != %q", ErrWorkerBrokerForeign, gotAbs, wantAbs)
	}
	if strings.TrimSpace(os.Getenv(envVolumeID)) != "" {
		if err := ValidateSharedMarker(wantAbs); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrWorkerBrokerForeign, err)
		}
	}

	return &ShareableWorkerBroker{URL: url, Token: tok, ClaimDir: wantAbs}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sameClaimPath(a, b string) bool {
	ca, err := filepath.Abs(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	cb, err := filepath.Abs(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(ca); err == nil {
		ca = r
	}
	if r, err := filepath.EvalSymlinks(cb); err == nil {
		cb = r
	}
	return filepath.Clean(ca) == filepath.Clean(cb)
}

// WriteShareableWorkerBrokerEnv forwards only the worker URL+token through the
// confidential contain env.list channel. Mint, coordinator, and seal keys are
// refused rather than copied.
func WriteShareableWorkerBrokerEnv(root string, cap *ShareableWorkerBroker) error {
	if cap == nil || strings.TrimSpace(cap.URL) == "" || strings.TrimSpace(cap.Token) == "" {
		return ErrWorkerBrokerPartial
	}
	path := ConfidentialWorkerBrokerEnvFile(root)
	if path == "" {
		return fmt.Errorf("provider: confidential worker env root required")
	}
	return security.UpsertEnvFileKeys(path, map[string]string{
		envFenceBrokerURL:   cap.URL,
		envFenceBrokerToken: cap.Token,
	})
}

// ApplyConfidentialWorkerBrokerEnv loads worker URL+token from contain env.list
// into this process when they are not already set. Mint/coordinator/seal keys
// in the file are ignored.
func ApplyConfidentialWorkerBrokerEnv(root string) error {
	path := ConfidentialWorkerBrokerEnvFile(root)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	url := strings.TrimSpace(os.Getenv(envFenceBrokerURL))
	tok := strings.TrimSpace(os.Getenv(envFenceBrokerToken))
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		key, val, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		switch key {
		case envFenceBrokerURL:
			if url == "" {
				url = strings.TrimSpace(val)
			}
		case envFenceBrokerToken:
			if tok == "" {
				tok = strings.TrimSpace(val)
			}
		case envFenceBrokerMintToken, envFenceCoordinator, envFenceMintCred, envVolumeID, envFenceMintFD:
			// Never promote coordinator/mint/seal material into the process.
			continue
		}
	}
	if url != "" {
		if err := os.Setenv(envFenceBrokerURL, url); err != nil {
			return err
		}
	}
	if tok != "" {
		if err := os.Setenv(envFenceBrokerToken, tok); err != nil {
			return err
		}
	}
	ScrubWorkerMintEnv()
	return nil
}

func applyConfidentialWorkerBrokerEnvFromProcess() error {
	root := strings.TrimSpace(os.Getenv("HERD_ROOT"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("HERD_REPO_ROOT"))
	}
	if root == "" {
		return nil
	}
	return ApplyConfidentialWorkerBrokerEnv(root)
}
