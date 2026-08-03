package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DiagnoseKindAuthReadiness performs bounded, non-leaking checks for whether
// a hosted author kind can start non-interactively through HostCreds.
// It does not spawn agents and never emits secret material.
func DiagnoseKindAuthReadiness(kind string) KindAuthDiagnosis {
	kind = strings.ToLower(strings.TrimSpace(kind))
	d := KindAuthDiagnosis{
		Kind:          kind,
		RequiredHosts: RequiredBrokerHostsForKind(kind),
		Class:         KindAuthExternal,
	}

	if ok, reason := PlatformHostCredsStatus(); !ok {
		d.Class = KindAuthPlatform
		d.Brokerable = false
		d.Reason = reason
		d.Blocker = fmt.Sprintf("FAC-170 BLOCKED: platform unsupported for HostCreds broker (%s)", reason)
		d.RecommendedAction = "run HostCreds broker on a supported unix-like coordinator host"
		return d
	}

	if len(d.RequiredHosts) == 0 {
		d.Class = KindAuthConfig
		d.Brokerable = false
		d.Reason = "unknown or out-of-scope harness kind — no RequiredBrokerHosts mapping"
		d.Blocker = fmt.Sprintf("FAC-170 BLOCKED: kind %q has no HostCreds host mapping (OpenCode/Ollama out of scope)", kind)
		d.RecommendedAction = "use grok, claude, or codex with API-key HostCreds"
		return d
	}

	creds := CoordinatorHostCredsFromEnv()
	d.HostCredsPresent = HostsPresent(creds)

	missing := []string{}
	for _, h := range d.RequiredHosts {
		if !hostCredPresent(creds, h) {
			missing = append(missing, h)
		}
	}

	// Host-side auth file presence (no content). Used to distinguish OAuth vs missing API key.
	home, _ := os.UserHomeDir()
	switch kind {
	case AuthorKindCodex:
		p := filepath.Join(home, ".codex", "auth.json")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			d.HostAuthFileHint = "host_codex_auth:present"
			if mode := sniffCodexAuthMode(p); mode != "" {
				d.HostAuthModeHint = mode
			}
		} else {
			d.HostAuthFileHint = "host_codex_auth:absent"
		}
	case AuthorKindClaude:
		d.HostAuthFileHint = "host_claude_creds:api_key_path"
	case AuthorKindGrok:
		d.HostAuthFileHint = "host_grok_creds:api_key_path"
	}

	if len(missing) == 0 {
		// Codex chatgpt OAuth cannot be brokered as an API-key oracle path.
		if kind == AuthorKindCodex && d.HostAuthModeHint == "chatgpt" && !hostCredPresent(creds, "api.openai.com") {
			d.Class = KindAuthExternal
			d.Brokerable = false
			d.Reason = "codex host auth_mode=chatgpt (OAuth); HostCreds oracle attaches API keys only"
			d.Blocker = "FAC-170 BLOCKED: codex requires OPENAI_API_KEY HostCreds in API-key mode — OAuth not brokerable"
			d.RecommendedAction = "export OPENAI_API_KEY for API-key codex; interactive browser login is forbidden"
			return d
		}
		// If openai key present even with chatgpt mode, API-key oracle path is brokerable.
		d.Class = KindAuthOK
		d.Brokerable = true
		d.Reason = "HostCreds present for required API hosts"
		d.Blocker = ""
		d.RecommendedAction = ""
		return d
	}

	d.Class = KindAuthExternal
	d.Brokerable = false
	d.Reason = fmt.Sprintf(
		"missing HostCreds for hosts %v (OPENAI_API_KEY/ANTHROPIC_API_KEY/XAI_API_KEY/HERD_HOST_CREDS unset in coordinator env)",
		missing,
	)
	d.Blocker = fmt.Sprintf(
		"FAC-170 BLOCKED: external credential state — kind=%s missing HostCreds hosts=%v; "+
			"worker HOME is scrubbed so host harness login files are unavailable; no interactive login UI",
		kind, missing,
	)
	d.RecommendedAction = "export coordinator API keys into HostCreds out-of-band store (env) before live harness proof; worker never receives real keys"
	return d
}

func hostCredPresent(creds map[string]string, host string) bool {
	if strings.TrimSpace(creds[host]) != "" {
		return true
	}
	for kh, v := range creds {
		if strings.EqualFold(kh, host) && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// sniffCodexAuthMode returns auth_mode if it is a short non-secret enum string.
func sniffCodexAuthMode(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	const key = `"auth_mode"`
	i := strings.Index(string(b), key)
	if i < 0 {
		return ""
	}
	rest := string(b[i+len(key):])
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	rest = strings.TrimSpace(rest[1:])
	if len(rest) < 2 || rest[0] != '"' {
		return ""
	}
	end := strings.IndexByte(rest[1:], '"')
	if end < 0 || end > 32 {
		return ""
	}
	mode := rest[1 : 1+end]
	switch mode {
	case "chatgpt", "api", "apikey", "api_key", "None", "none":
		return mode
	default:
		if len(mode) <= 16 && mode != "" {
			return mode
		}
		return ""
	}
}

// FormatKindAuthBlocker is a single-line coordinator packet (no secrets).
func FormatKindAuthBlocker(d KindAuthDiagnosis) string {
	return fmt.Sprintf(
		"%s class=%s brokerable=%v hosts_required=%v hosts_creds=%v auth_file=%s auth_mode=%s action=%s",
		d.Blocker, d.Class, d.Brokerable, d.RequiredHosts, d.HostCredsPresent,
		d.HostAuthFileHint, d.HostAuthModeHint, d.RecommendedAction,
	)
}

// RedactSecrets strips known secret patterns from a diagnostic string.
// Used as a belt-and-suspenders guard on diagnosis packets.
func RedactSecrets(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lower := strings.ToLower(s)
	for i := 0; i < len(s); {
		// Bearer <token>
		if i+7 <= len(s) && lower[i:i+7] == "bearer " {
			j := i + 7
			for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' && s[j] != '"' && s[j] != '\'' && s[j] != '`' {
				j++
			}
			// Skip already-redacted placeholders.
			token := s[i+7 : j]
			if token == "[REDACTED]" {
				b.WriteString(s[i:j])
				i = j
				continue
			}
			b.WriteString("Bearer [REDACTED]")
			i = j
			continue
		}
		// sk-... API key shape
		if i+3 <= len(s) && s[i:i+3] == "sk-" {
			j := i + 3
			if j < len(s) && s[j] == '[' {
				// already sk-[REDACTED]
				b.WriteByte(s[i])
				i++
				continue
			}
			for j < len(s) && isSecretChar(s[j]) {
				j++
			}
			if j > i+3 {
				b.WriteString("sk-[REDACTED]")
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isSecretChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}
