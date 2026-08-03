package security

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CredentialAuthority is the production out-of-band secret holder.
//
// Public methods NEVER return Authorization material. Injection is performed
// only via InjectAuthorization into an http.Header the oracle owns.
//
// Production implementations resolve OS secret-service / 1Password handles.
// Raw process environment API keys are NOT a production authority
// (same-UID workers can inspect parent env/proc).
type CredentialAuthority interface {
	// Class is "keychain", "op", "test", or "none".
	Class() string
	// Durable reports whether credentials survive process restart via re-resolve.
	Durable() bool
	// Has reports presence of real material for host (never the value).
	Has(host string) bool
	// Hosts returns host names with material (never values).
	Hosts() []string
	// InstallFromHandle binds host → opaque handle and resolves material into
	// broker-private memory. Handle is not the secret (e.g. keychain:…, op://…).
	InstallFromHandle(host, handle string) error
	// RotateFromHandle replaces material by re-resolving a handle (durable backends).
	RotateFromHandle(host, handle string) error
	// Revoke drops material for host.
	Revoke(host string) error
	// InjectAuthorization sets Authorization on hdr from internal material.
	// Never returns the secret string to the caller.
	InjectAuthorization(host string, hdr http.Header) error
	// Handles returns host→handle map (handles only, not secrets) for restart rebind.
	Handles() map[string]string
}

// Handle env for production (NOT secrets): HERD_HOSTCREDS_HANDLES="api.x.ai=keychain:herd.xai;api.openai.com=op://Vault/item/field"
const envHostCredsHandles = "HERD_HOSTCREDS_HANDLES"

// ParseHandlesEnv parses HERD_HOSTCREDS_HANDLES (handles only, never secret values).
func ParseHandlesEnv(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(part[:i]))
		handle := strings.TrimSpace(part[i+1:])
		if host == "" || handle == "" {
			continue
		}
		// Reject env smuggling of raw Bearer secrets as "handles".
		if IsDummyCredential(handle) || strings.HasPrefix(strings.ToLower(handle), "bearer ") {
			continue
		}
		if strings.HasPrefix(handle, "sk-") {
			continue
		}
		out[host] = handle
	}
	return out
}

// EnvRawAPIKeysPresent reports whether insecure raw API key env vars are set.
// Presence blocks treating the process as production-brokerable via env alone.
func EnvRawAPIKeysPresent() bool {
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "HERD_HOST_CREDS"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			// Dummy sentinel in these vars is OK (public).
			if IsDummyCredential(os.Getenv(k)) {
				continue
			}
			return true
		}
	}
	return false
}

// --- Test vault (tests only; not production durable authority) ---

// TestCredentialVault holds secrets in-process for unit tests only.
// It is NOT a production authority: Durable() is false for cross-process
// restart; use HandleAuthority (keychain/op) for production.
//
// There is no public Get/Snapshot. Tests install via InstallTestSecret.
type TestCredentialVault struct {
	mu      sync.RWMutex
	creds   map[string]string // host → auth (package-private)
	handles map[string]string
}

// NewTestCredentialVault creates a tests-only vault. Production must not use this.
func NewTestCredentialVault() *TestCredentialVault {
	return &TestCredentialVault{
		creds:   map[string]string{},
		handles: map[string]string{},
	}
}

func (v *TestCredentialVault) Class() string { return "test" }
func (v *TestCredentialVault) Durable() bool { return false } // in-process only

func (v *TestCredentialVault) Has(host string) bool {
	if v == nil {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	a := v.creds[strings.ToLower(strings.TrimSpace(host))]
	return a != "" && !IsDummyCredential(a)
}

func (v *TestCredentialVault) Hosts() []string {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	var out []string
	for h, a := range v.creds {
		if a != "" && !IsDummyCredential(a) {
			out = append(out, h)
		}
	}
	return sortHosts(out)
}

// InstallTestSecret is tests-only. Validates material; never used by production CLI.
func (v *TestCredentialVault) InstallTestSecret(host, authorization string) error {
	if v == nil {
		return fmt.Errorf("nil vault")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return &BlockedError{Reason: BlockBadHost, Code: "host_empty"}
	}
	if err := ValidateAuthorizationMaterial(authorization); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.creds == nil {
		v.creds = map[string]string{}
	}
	v.creds[host] = strings.TrimSpace(authorization)
	return nil
}

func (v *TestCredentialVault) InstallFromHandle(host, handle string) error {
	// Test vault does not resolve external handles; use InstallTestSecret.
	return &BlockedError{Reason: BlockHandleUnresolved, Code: "test_vault_no_handles"}
}

func (v *TestCredentialVault) RotateFromHandle(host, handle string) error {
	return v.InstallFromHandle(host, handle)
}

// RotateTestSecret is tests-only rotation (in-process).
func (v *TestCredentialVault) RotateTestSecret(host, authorization string) error {
	return v.InstallTestSecret(host, authorization)
}

func (v *TestCredentialVault) Revoke(host string) error {
	if v == nil {
		return fmt.Errorf("nil vault")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.creds, host)
	delete(v.handles, host)
	return nil
}

func (v *TestCredentialVault) InjectAuthorization(host string, hdr http.Header) error {
	if v == nil || hdr == nil {
		return fmt.Errorf("nil vault/header")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	v.mu.RLock()
	auth := v.creds[host]
	v.mu.RUnlock()
	if auth == "" {
		return &BlockedError{Reason: BlockMissingCreds, Code: "missing:" + host, HostsRequired: []string{host}}
	}
	if IsDummyCredential(auth) {
		return &BlockedError{Reason: BlockDummyUpstream, Code: "auth_dummy"}
	}
	if err := ValidateAuthorizationMaterial(auth); err != nil {
		return err
	}
	hdr.Set("Authorization", auth)
	return nil
}

func (v *TestCredentialVault) Handles() map[string]string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := map[string]string{}
	for h, handle := range v.handles {
		out[h] = handle
	}
	return out
}

// Ensure TestCredentialVault implements CredentialAuthority.
var _ CredentialAuthority = (*TestCredentialVault)(nil)

// --- Handle-based production authority ---

// HandleAuthority resolves keychain: and op:// handles into broker-private memory.
// Handles may appear in coordinator config/env; secret bytes never do.
type HandleAuthority struct {
	mu      sync.RWMutex
	creds   map[string]string // resolved material (private)
	handles map[string]string // host → handle
	// resolve is overridable for tests.
	resolve func(handle string) (string, error)
}

// NewHandleAuthority creates a durable handle-backed authority.
func NewHandleAuthority() *HandleAuthority {
	return &HandleAuthority{
		creds:   map[string]string{},
		handles: map[string]string{},
		resolve: resolveSecretHandle,
	}
}

// NewHandleAuthorityFromEnv loads HERD_HOSTCREDS_HANDLES and resolves each handle.
// Fails closed if any handle cannot be resolved. Ignores raw API key env vars.
func NewHandleAuthorityFromEnv() (*HandleAuthority, error) {
	a := NewHandleAuthority()
	handles := ParseHandlesEnv(os.Getenv(envHostCredsHandles))
	if len(handles) == 0 {
		return a, nil
	}
	for host, handle := range handles {
		if err := a.InstallFromHandle(host, handle); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *HandleAuthority) Class() string {
	if a == nil {
		return "none"
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, h := range a.handles {
		if strings.HasPrefix(h, "op://") {
			return "op"
		}
		if strings.HasPrefix(h, "keychain:") {
			return "keychain"
		}
	}
	if len(a.handles) > 0 {
		return "handle"
	}
	return "none"
}

func (a *HandleAuthority) Durable() bool { return true }

func (a *HandleAuthority) Has(host string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	auth := a.creds[strings.ToLower(strings.TrimSpace(host))]
	return auth != "" && !IsDummyCredential(auth)
}

func (a *HandleAuthority) Hosts() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []string
	for h, auth := range a.creds {
		if auth != "" && !IsDummyCredential(auth) {
			out = append(out, h)
		}
	}
	return sortHosts(out)
}

func (a *HandleAuthority) InstallFromHandle(host, handle string) error {
	if a == nil {
		return fmt.Errorf("nil authority")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	handle = strings.TrimSpace(handle)
	if host == "" || handle == "" {
		return &BlockedError{Reason: BlockHandleUnresolved, Code: "handle_empty"}
	}
	if strings.HasPrefix(strings.ToLower(handle), "bearer ") || strings.HasPrefix(handle, "sk-") || IsDummyCredential(handle) {
		return &BlockedError{Reason: BlockEnvNotAuthority, Code: "handle_looks_like_secret"}
	}
	resolve := a.resolve
	if resolve == nil {
		resolve = resolveSecretHandle
	}
	material, err := resolve(handle)
	if err != nil {
		return &BlockedError{Reason: BlockHandleUnresolved, Code: "resolve_failed"}
	}
	// Normalize to Authorization header form if raw token.
	auth := strings.TrimSpace(material)
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		auth = "Bearer " + auth
	}
	if err := ValidateAuthorizationMaterial(auth); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.creds == nil {
		a.creds = map[string]string{}
	}
	if a.handles == nil {
		a.handles = map[string]string{}
	}
	a.creds[host] = auth
	a.handles[host] = handle
	return nil
}

func (a *HandleAuthority) RotateFromHandle(host, handle string) error {
	return a.InstallFromHandle(host, handle)
}

func (a *HandleAuthority) Revoke(host string) error {
	if a == nil {
		return fmt.Errorf("nil authority")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.creds, host)
	delete(a.handles, host)
	return nil
}

func (a *HandleAuthority) InjectAuthorization(host string, hdr http.Header) error {
	if a == nil || hdr == nil {
		return fmt.Errorf("nil authority/header")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	a.mu.RLock()
	auth := a.creds[host]
	a.mu.RUnlock()
	if auth == "" {
		return &BlockedError{Reason: BlockMissingCreds, Code: "missing:" + host, HostsRequired: []string{host}}
	}
	if err := ValidateAuthorizationMaterial(auth); err != nil {
		return err
	}
	hdr.Set("Authorization", auth)
	return nil
}

func (a *HandleAuthority) Handles() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[string]string{}
	for h, handle := range a.handles {
		out[h] = handle
	}
	return out
}

// ReResolveAll re-loads material from stored handles (restart durability).
func (a *HandleAuthority) ReResolveAll() error {
	if a == nil {
		return fmt.Errorf("nil authority")
	}
	a.mu.RLock()
	handles := map[string]string{}
	for h, handle := range a.handles {
		handles[h] = handle
	}
	a.mu.RUnlock()
	for h, handle := range handles {
		if err := a.InstallFromHandle(h, handle); err != nil {
			return err
		}
	}
	return nil
}

var _ CredentialAuthority = (*HandleAuthority)(nil)

// resolveSecretHandle resolves keychain:name or op://path into secret material.
// Material never logged.
func resolveSecretHandle(handle string) (string, error) {
	handle = strings.TrimSpace(handle)
	switch {
	case strings.HasPrefix(handle, "op://"):
		// 1Password CLI — secret stays in this process stdout pipe only.
		cmd := exec.Command("op", "read", handle)
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	case strings.HasPrefix(handle, "keychain:"):
		service := strings.TrimPrefix(handle, "keychain:")
		if service == "" {
			return "", fmt.Errorf("empty keychain service")
		}
		// macOS security(1) generic password; account fixed as herd-hostcreds.
		cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", "herd-hostcreds", "-w")
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	default:
		return "", fmt.Errorf("unknown handle scheme")
	}
}

func sortHosts(in []string) []string {
	out := append([]string(nil), in...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
