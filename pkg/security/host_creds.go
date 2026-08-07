package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CoordinatorHostCredsFromEnv collects model credentials that must live only
// on the coordinator/broker (never in agent env). Keys are upstream hosts;
// values are full Authorization header values.
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
	// Optional explicit map: HERD_HOST_CREDS="host=Bearer tok;host2=Bearer tok2"
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
				out[host] = auth
			}
		}
	}
	return out
}

// WireBrokerHostCredsAndCA seeds coordinator HostCreds into the broker and
// writes the public CA PEM under worktree for agent SSL_CERT_FILE trust.
//
// Fail-closed (FAC-133 re-admission): live inject must succeed when a durable
// broker is running; empty CA after seed is an error. Never report success
// without readback proving credentials are installed and CAPEM is present.
func WireBrokerHostCredsAndCA(bl *BrokerLaunch, worktree string, hostCreds map[string]string) (caPath string, err error) {
	if bl == nil {
		return "", fmt.Errorf("wire host creds: nil broker launch")
	}
	if worktree == "" {
		return "", fmt.Errorf("wire host creds: worktree required")
	}
	if len(hostCreds) == 0 {
		// No secrets to install — still require CA when durable control exists
		// only if we claim HostCreds MITM; without creds, CA optional.
		return publishBrokerCAStrict(bl, worktree, false)
	}
	if bl.Inline != nil {
		for h, a := range hostCreds {
			bl.Inline.SetHostCredential(h, a)
		}
		if err := bl.Inline.EnsureCA(); err != nil {
			return "", fmt.Errorf("wire host creds: EnsureCA: %w", err)
		}
		// Readback: every host must be present on the inline broker.
		for h, want := range hostCreds {
			got := bl.Inline.hostCred(h)
			if got != want {
				return "", fmt.Errorf("wire host creds: readback miss for %s", h)
			}
		}
		if len(bl.Inline.CAPEM()) == 0 {
			return "", fmt.Errorf("wire host creds: empty CAPEM after EnsureCA")
		}
		return writeAgentCAPEM(worktree, bl.Inline.CAPEM())
	}
	if bl.ControlPath == "" {
		return "", fmt.Errorf("wire host creds: no control path for durable broker")
	}
	if err := SeedCoordinatorHostCreds(bl.ControlPath, hostCreds); err != nil {
		return "", fmt.Errorf("wire host creds: seed: %w", err)
	}
	ctrl, err := ReadBrokerControlState(bl.ControlPath)
	if err != nil {
		return "", fmt.Errorf("wire host creds: read control after seed: %w", err)
	}
	// Live inject is mandatory when broker process is up (PID known or control answers).
	if err := InjectHostCredsLive(ctrl, hostCreds); err != nil {
		// Fail closed: preseed alone is insufficient once StartBrokerForLaunch returned.
		return "", fmt.Errorf("wire host creds: live inject failed (fail-closed): %w", err)
	}
	// Re-read control state and prove HostCreds + CAPEM present.
	ctrl2, err := ReadBrokerControlState(bl.ControlPath)
	if err != nil {
		return "", fmt.Errorf("wire host creds: control readback: %w", err)
	}
	for h := range hostCreds {
		if ctrl2.HostCreds == nil || strings.TrimSpace(ctrl2.HostCreds[strings.ToLower(h)]) == "" {
			// HostCreds may be stored lowercased at seed time.
			found := false
			for kh, v := range ctrl2.HostCreds {
				if strings.EqualFold(kh, h) && strings.TrimSpace(v) != "" {
					found = true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("wire host creds: live readback missing host %s", h)
			}
		}
	}
	if strings.TrimSpace(ctrl2.CAPEM) == "" {
		// Try EnsureCA via control if CAPEM not yet written.
		return "", fmt.Errorf("wire host creds: CAPEM missing after live inject (fail-closed)")
	}
	return writeAgentCAPEM(worktree, []byte(ctrl2.CAPEM))
}

func publishBrokerCAStrict(bl *BrokerLaunch, worktree string, required bool) (string, error) {
	if bl.Inline != nil {
		if err := bl.Inline.EnsureCA(); err != nil {
			if required {
				return "", err
			}
			return "", nil
		}
		if len(bl.Inline.CAPEM()) == 0 {
			if required {
				return "", fmt.Errorf("empty CAPEM")
			}
			return "", nil
		}
		return writeAgentCAPEM(worktree, bl.Inline.CAPEM())
	}
	if bl.ControlPath == "" {
		if required {
			return "", fmt.Errorf("no control path")
		}
		return "", nil
	}
	ctrl, err := ReadBrokerControlState(bl.ControlPath)
	if err != nil {
		if required {
			return "", fmt.Errorf("control state read: %w", err)
		}
		return "", nil
	}
	if strings.TrimSpace(ctrl.CAPEM) == "" {
		if required {
			return "", fmt.Errorf("empty CAPEM in control state")
		}
		return "", nil
	}
	return writeAgentCAPEM(worktree, []byte(ctrl.CAPEM))
}

func writeAgentCAPEM(worktree string, pem []byte) (string, error) {
	if len(pem) == 0 {
		return "", fmt.Errorf("writeAgentCAPEM: empty pem")
	}
	dir := filepath.Join(worktree, ".herd", "contain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "broker-ca.pem")
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
