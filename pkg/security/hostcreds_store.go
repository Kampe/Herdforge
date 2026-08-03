package security

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// SecretStore holds coordinator-only credentials out-of-band.
// Values are Authorization header material (e.g. "Bearer sk-...").
// Implementations must never be readable from worker env, argv, or worktree.
type SecretStore interface {
	// Get returns the Authorization value for host, or "" if absent/revoked.
	Get(host string) string
	// Set installs or rotates the Authorization value for host.
	Set(host, authorization string) error
	// Delete revokes credentials for host.
	Delete(host string) error
	// Hosts returns hosts with non-empty credentials (names only).
	Hosts() []string
	// Snapshot returns a deep copy of host→auth for broker seed (coordinator only).
	Snapshot() map[string]string
}

// MemorySecretStore is an in-process out-of-band store (tests + coordinator).
type MemorySecretStore struct {
	mu    sync.RWMutex
	creds map[string]string
}

// NewMemorySecretStore creates an empty coordinator-side store.
func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{creds: map[string]string{}}
}

func (s *MemorySecretStore) Get(host string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creds[strings.ToLower(strings.TrimSpace(host))]
}

func (s *MemorySecretStore) Set(host, authorization string) error {
	if s == nil {
		return fmt.Errorf("nil secret store")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	authorization = strings.TrimSpace(authorization)
	if host == "" || authorization == "" {
		return fmt.Errorf("host and authorization required")
	}
	// Dummy CLI sentinels must never become accepted upstream credentials.
	if IsDummyCredential(authorization) {
		return &BlockedError{
			Reason: BlockDummyUpstream,
			Detail: "cannot store dummy sentinel as HostCreds",
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		s.creds = map[string]string{}
	}
	s.creds[host] = authorization
	return nil
}

func (s *MemorySecretStore) Delete(host string) error {
	if s == nil {
		return fmt.Errorf("nil secret store")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, host)
	return nil
}

func (s *MemorySecretStore) Hosts() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.creds))
	for h, v := range s.creds {
		if strings.TrimSpace(v) != "" {
			out = append(out, h)
		}
	}
	return out
}

func (s *MemorySecretStore) Snapshot() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.creds))
	for h, v := range s.creds {
		out[h] = v
	}
	return out
}

// CoordinatorHostCredsFromEnv collects model credentials that must live only
// on the coordinator/broker (never in agent env). Keys are upstream hosts;
// values are full Authorization header values.
//
// Sources (first-match per host, later sources do not overwrite earlier):
//   - ANTHROPIC_API_KEY → api.anthropic.com
//   - OPENAI_API_KEY → api.openai.com
//   - XAI_API_KEY → api.x.ai
//   - HERD_HOST_CREDS="host=Bearer tok;host2=Bearer tok2"
func CoordinatorHostCredsFromEnv() map[string]string {
	out := map[string]string{}
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); v != "" {
		out["api.anthropic.com"] = "Bearer " + v
	}
	if v := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); v != "" {
		out["api.openai.com"] = "Bearer " + v
	}
	if v := strings.TrimSpace(os.Getenv("XAI_API_KEY")); v != "" {
		out["api.x.ai"] = "Bearer " + v
	}
	if raw := strings.TrimSpace(os.Getenv("HERD_HOST_CREDS")); raw != "" {
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
			auth := strings.TrimSpace(part[i+1:])
			if host != "" && auth != "" {
				// Env map keys win over HERD_HOST_CREDS if already set.
				if _, exists := out[host]; !exists {
					out[host] = auth
				}
			}
		}
	}
	return out
}

// LoadEnvIntoStore copies coordinator env HostCreds into store (host names only exposed via Hosts()).
// Dummy CLI sentinels are skipped (never become upstream credentials).
func LoadEnvIntoStore(store SecretStore) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	for h, a := range CoordinatorHostCredsFromEnv() {
		if IsDummyCredential(a) {
			continue
		}
		if err := store.Set(h, a); err != nil {
			// Skip dummy rejections; surface other errors.
			if be, ok := err.(*BlockedError); ok && be.Reason == BlockDummyUpstream {
				continue
			}
			return err
		}
	}
	return nil
}

// HostsPresent returns sorted host names with non-empty values (never values).
func HostsPresent(creds map[string]string) []string {
	out := make([]string, 0, len(creds))
	for h, v := range creds {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.ToLower(h))
		}
	}
	// Deterministic order for diagnosis packets.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
