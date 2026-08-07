package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session attestation errors (standing agent reuse must fail closed).
var (
	ErrSessionUnattested = fmt.Errorf("security: standing agent session not attested under current containment (fail-closed)")
	ErrSessionStale      = fmt.Errorf("security: standing agent session policy/profile digest is stale (fail-closed)")
	ErrSessionIdentity   = fmt.Errorf("security: live session identity does not match attestation (fail-closed)")
	ErrSessionRetire     = fmt.Errorf("security: failed to retire stale standing session (fail-closed)")
)

// SessionAttestation records a launch bound to the live Herdr agent_session,
// tab/pane, task/ref, and lease generation. Paths are repo-relative only.
type SessionAttestation struct {
	// Generation is a cryptographically unique launch generation (rand, fail-closed).
	Generation string `json:"generation"`
	// AgentSessionID is the actual Herdr agent_session.value (not a tab hash).
	AgentSessionID string `json:"agent_session_id"`
	// TaskRef is the board/task reference bound at launch.
	TaskRef string `json:"task_ref"`
	// LeaseGeneration is the control-plane lease generation (required).
	LeaseGeneration string `json:"lease_generation"`

	PolicyDigest string `json:"policy_digest"`
	AgentName    string `json:"agent_name"`
	Kind         string `json:"kind"`
	Role         string `json:"role"`
	Network      string `json:"network"`
	CWDRel       string `json:"cwd_rel"`
	Containment  string `json:"containment"`

	TabID  string `json:"tab_id"`
	PaneID string `json:"pane_id"`

	LaunchedAt time.Time `json:"launched_at"`
}

// LiveAgentIdentity is the currently running Herdr agent surface.
type LiveAgentIdentity struct {
	Name           string
	Kind           string
	TabID          string
	PaneID         string
	Status         string
	AgentSessionID string // herdr agent_session.value
	TerminalID     string
}

// PolicyDigestInput is the canonical material hashed into a policy digest.
type PolicyDigestInput struct {
	Role              string   `json:"role"`
	Authority         string   `json:"authority"`
	Network           string   `json:"network"`
	NetworkAllowHosts []string `json:"network_allow_hosts"`
	AllowedTools      []string `json:"allowed_tools"`
	DeniedTools       []string `json:"denied_tools"`
	CWDRel            string   `json:"cwd_rel"`
	RepoIdentity      string   `json:"repo_identity"`
	ExclusivePackages bool     `json:"exclusive_packages"`
	PackageAllowlist  []string `json:"package_allowlist"`
	ExternalLinks     string   `json:"external_links"`
}

// ComputePolicyDigest returns a stable hex SHA-256 of the launch policy surface.
func ComputePolicyDigest(policy *LaunchPolicy) (string, error) {
	if policy == nil {
		return "", ErrUnknownPolicy
	}
	in := PolicyDigestInput{
		Role:              strings.ToLower(policy.Role),
		Authority:         string(policy.Authority),
		Network:           strings.ToLower(policy.Network),
		NetworkAllowHosts: sortedCopy(policy.NetworkAllowHosts),
		AllowedTools:      sortedCopy(policy.AllowedTools),
		DeniedTools:       sortedCopy(policy.DeniedTools),
		CWDRel:            RelIdentity(policy.FilesystemRoot, policy.SharedCheckout),
		RepoIdentity:      policy.RepoIdentity,
		ExclusivePackages: policy.ExclusivePackages,
		PackageAllowlist:  sortedCopy(policy.PackageAllowlist),
		ExternalLinks:     string(policy.ExternalLinks),
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// SessionAttestationPath is the durable attestation file (under shared .herd).
func SessionAttestationPath(sharedCheckout, agentName string) string {
	return filepath.Join(sharedCheckout, ".herd", "sessions", sanitizeAgentName(agentName)+".json")
}

func sanitizeAgentName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// WriteSessionAttestation persists attestation with atomic rename + fsync (fail-closed).
func WriteSessionAttestation(sharedCheckout string, att SessionAttestation) error {
	if strings.TrimSpace(sharedCheckout) == "" {
		return fmt.Errorf("%w: shared checkout required for attestation", ErrUnknownPolicy)
	}
	if err := att.validateComplete(); err != nil {
		return err
	}
	path := SessionAttestationPath(sharedCheckout, att.AgentName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if att.LaunchedAt.IsZero() {
		att.LaunchedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(att, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0o600)
}

func (att SessionAttestation) validateComplete() error {
	switch {
	case att.AgentName == "":
		return fmt.Errorf("%w: agent_name required", ErrUnknownPolicy)
	case att.PolicyDigest == "":
		return fmt.Errorf("%w: policy_digest required", ErrUnknownPolicy)
	case att.Generation == "":
		return fmt.Errorf("%w: generation required", ErrUnknownPolicy)
	case att.AgentSessionID == "":
		return fmt.Errorf("%w: agent_session_id required", ErrUnknownPolicy)
	case att.TabID == "" || att.PaneID == "":
		return fmt.Errorf("%w: tab/pane required", ErrUnknownPolicy)
	case att.Kind == "" || att.Role == "" || att.Network == "" || att.CWDRel == "" || att.Containment == "":
		return fmt.Errorf("%w: kind/role/network/cwd/containment required", ErrUnknownPolicy)
	case att.TaskRef == "":
		return fmt.Errorf("%w: task_ref required", ErrUnknownPolicy)
	case att.LeaseGeneration == "":
		return fmt.Errorf("%w: lease_generation required", ErrUnknownPolicy)
	}
	return nil
}

// atomicWriteFile writes via temp file, fsync, rename, and directory fsync.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".attestation-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("attestation fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("attestation dir open for fsync: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("attestation dir fsync: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("attestation dir close: %w", err)
	}
	return nil
}

// LoadSessionAttestation reads a previously written attestation.
func LoadSessionAttestation(sharedCheckout, agentName string) (*SessionAttestation, error) {
	path := SessionAttestationPath(sharedCheckout, agentName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionUnattested, err)
	}
	var att SessionAttestation
	if err := json.Unmarshal(b, &att); err != nil {
		return nil, fmt.Errorf("%w: corrupt attestation", ErrSessionUnattested)
	}
	if err := att.validateComplete(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionUnattested, err)
	}
	return &att, nil
}

// RequireSessionAttestation fails closed unless every attested field matches
// live Herdr authority and the current policy digest.
func RequireSessionAttestation(sharedCheckout, agentName string, policy *LaunchPolicy, live *LiveAgentIdentity, wantTaskRef, wantLease string) (*SessionAttestation, error) {
	if live == nil {
		return nil, fmt.Errorf("%w: live identity required", ErrSessionUnattested)
	}
	if strings.TrimSpace(live.TabID) == "" || strings.TrimSpace(live.PaneID) == "" {
		return nil, fmt.Errorf("%w: live tab/pane required", ErrSessionUnattested)
	}
	if strings.TrimSpace(live.AgentSessionID) == "" {
		return nil, fmt.Errorf("%w: live agent_session_id required", ErrSessionUnattested)
	}
	att, err := LoadSessionAttestation(sharedCheckout, agentName)
	if err != nil {
		return nil, err
	}
	digest, err := ComputePolicyDigest(policy)
	if err != nil {
		return nil, err
	}
	if att.PolicyDigest != digest {
		return att, fmt.Errorf("%w: policy digest mismatch", ErrSessionStale)
	}
	if !strings.EqualFold(att.AgentName, agentName) {
		return att, fmt.Errorf("%w: agent name", ErrSessionIdentity)
	}
	if live.Name != "" && !strings.EqualFold(att.AgentName, live.Name) {
		return att, fmt.Errorf("%w: live name", ErrSessionIdentity)
	}
	if live.Kind != "" && !strings.EqualFold(att.Kind, live.Kind) {
		return att, fmt.Errorf("%w: kind", ErrSessionIdentity)
	}
	if att.TabID != live.TabID || att.PaneID != live.PaneID {
		return att, fmt.Errorf("%w: tab/pane (stale standing tab)", ErrSessionIdentity)
	}
	// Session match: real session ids must equal. Session-less live-agent:
	// bindings match when the live agent has empty session and tab/pane/name
	// already matched above (att.AgentSessionID is live-agent:name|tab|pane).
	if att.AgentSessionID != live.AgentSessionID {
		if !(strings.HasPrefix(att.AgentSessionID, "live-agent:") && strings.TrimSpace(live.AgentSessionID) == "") {
			return att, fmt.Errorf("%w: agent_session_id", ErrSessionIdentity)
		}
	}
	if wantTaskRef != "" && att.TaskRef != wantTaskRef {
		return att, fmt.Errorf("%w: task_ref", ErrSessionIdentity)
	}
	if wantLease != "" && att.LeaseGeneration != wantLease {
		return att, fmt.Errorf("%w: lease_generation", ErrSessionIdentity)
	}
	if policy != nil {
		if !strings.EqualFold(att.Role, policy.Role) {
			return att, fmt.Errorf("%w: role", ErrSessionStale)
		}
		if !strings.EqualFold(att.Network, policy.Network) {
			return att, fmt.Errorf("%w: network", ErrSessionStale)
		}
		cwdRel := RelIdentity(policy.FilesystemRoot, policy.SharedCheckout)
		if att.CWDRel != cwdRel {
			return att, fmt.Errorf("%w: cwd", ErrSessionStale)
		}
	}
	// Match ValidateFleetAttestation: "skipped" is test-only and must never
	// satisfy standing-agent reuse (SkipContainment is FORBIDDEN in production).
	if att.Containment == "" || att.Containment == "none" || att.Containment == "skipped" {
		return att, fmt.Errorf("%w: containment", ErrSessionIdentity)
	}
	if att.LaunchedAt.IsZero() {
		return att, fmt.Errorf("%w: launched_at", ErrSessionIdentity)
	}
	return att, nil
}

// NewGeneration returns a unique launch generation or errors (no wall-clock fallback).
func NewGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("security: crypto/rand failed for generation: %w", err)
	}
	return "gen-" + hex.EncodeToString(b[:]), nil
}
