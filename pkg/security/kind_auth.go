package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DiagnoseKindAuthReadiness performs bounded, non-leaking checks.
// Production brokerable requires handle-backed authority presence (not raw env keys).
// Optional auth parameter: when nil, uses handles from HERD_HOSTCREDS_HANDLES only.
func DiagnoseKindAuthReadiness(kind string) KindAuthDiagnosis {
	return DiagnoseKindAuthReadinessWith(kind, nil)
}

// DiagnoseKindAuthReadinessWith uses an explicit authority when provided.
func DiagnoseKindAuthReadinessWith(kind string, auth CredentialAuthority) KindAuthDiagnosis {
	kind = strings.ToLower(strings.TrimSpace(kind))
	d := KindAuthDiagnosis{
		Kind:           kind,
		RequiredHosts:  RequiredBrokerHostsForKind(kind),
		Class:          KindAuthExternal,
		AuthorityClass: "none",
	}

	if ok, reason := PlatformHostCredsStatus(); !ok {
		d.Class = KindAuthPlatform
		d.Brokerable = false
		d.ReasonCode = "platform_unsupported"
		d.Blocker = "FAC-170 BLOCKED: platform unsupported for HostCreds oracle"
		d.RecommendedAction = "run HostCreds broker on a supported unix-like coordinator host"
		_ = reason
		return d
	}

	if len(d.RequiredHosts) == 0 {
		d.Class = KindAuthConfig
		d.Brokerable = false
		d.ReasonCode = "unknown_kind"
		d.Blocker = fmt.Sprintf("FAC-170 BLOCKED: kind %q has no HostCreds mapping (OpenCode out of scope)", kind)
		d.RecommendedAction = "use grok, claude, or codex with handle-backed HostCreds"
		return d
	}

	// Host-side auth file hints (no content).
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
		d.HostAuthFileHint = "host_claude_creds:handle_path"
	case AuthorKindGrok:
		d.HostAuthFileHint = "host_grok_creds:handle_path"
	}

	// Raw env keys are NOT production authority.
	if EnvRawAPIKeysPresent() {
		d.ReasonCode = "env_not_production_authority"
		// Continue — handles may still make us brokerable.
	}

	if auth == nil {
		// Presence from handles env without resolving secrets when possible.
		handles := ParseHandlesEnv(os.Getenv(envHostCredsHandles))
		var present []string
		for _, h := range d.RequiredHosts {
			if _, ok := handles[h]; ok {
				present = append(present, h)
			}
		}
		d.HostCredsPresent = sortHosts(present)
		if len(handles) > 0 {
			d.AuthorityClass = "handle"
		}
		// Try resolve only if handles cover required hosts (may fail offline).
		if len(present) == len(d.RequiredHosts) && len(d.RequiredHosts) > 0 {
			ha, err := NewHandleAuthorityFromEnv()
			if err == nil && authorityCovers(ha, d.RequiredHosts) {
				auth = ha
			}
		}
	}

	if auth != nil {
		d.AuthorityClass = auth.Class()
		d.HostCredsPresent = auth.Hosts()
		if authorityCovers(auth, d.RequiredHosts) {
			if kind == AuthorKindCodex && d.HostAuthModeHint == "chatgpt" && !auth.Has("api.openai.com") {
				d.Class = KindAuthExternal
				d.Brokerable = false
				d.ReasonCode = "codex_oauth_unbrokerable"
				d.Blocker = "FAC-170 BLOCKED: codex OAuth not brokerable; need API-key handle"
				d.RecommendedAction = "install OPENAI API key via keychain:/op:// handle; no browser login"
				return d
			}
			d.Class = KindAuthOK
			d.Brokerable = true
			d.ReasonCode = "ok"
			d.Blocker = ""
			d.RecommendedAction = ""
			return d
		}
	}

	d.Class = KindAuthExternal
	d.Brokerable = false
	if d.ReasonCode == "" {
		d.ReasonCode = "missing_handle_creds"
	}
	d.Blocker = fmt.Sprintf(
		"FAC-170 BLOCKED: kind=%s missing handle-backed HostCreds hosts=%v; raw env keys are not production authority",
		kind, d.RequiredHosts,
	)
	d.RecommendedAction = "set HERD_HOSTCREDS_HANDLES to keychain: or op:// handles; never export raw API keys for workers"
	return d
}

func authorityCovers(auth CredentialAuthority, required []string) bool {
	if auth == nil {
		return false
	}
	for _, h := range required {
		if !auth.Has(h) {
			return false
		}
	}
	return true
}

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
	rest := strings.TrimSpace(string(b[i+len(key):]))
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
		if len(mode) <= 16 {
			return mode
		}
		return ""
	}
}

// FormatKindAuthBlocker is a single-line coordinator packet (no secrets).
func FormatKindAuthBlocker(d KindAuthDiagnosis) string {
	return fmt.Sprintf(
		"%s class=%s brokerable=%v hosts_required=%v hosts_creds=%v authority=%s reason_code=%s auth_file=%s action=%s",
		d.Blocker, d.Class, d.Brokerable, d.RequiredHosts, d.HostCredsPresent,
		d.AuthorityClass, d.ReasonCode, d.HostAuthFileHint, d.RecommendedAction,
	)
}

// RedactSecrets strips bearer/sk/api-key/partial/base64-ish secret shapes.
func RedactSecrets(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lower := strings.ToLower(s)
	for i := 0; i < len(s); {
		if i+7 <= len(s) && lower[i:i+7] == "bearer " {
			j := i + 7
			for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' && s[j] != '"' && s[j] != '\'' {
				j++
			}
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
		// sk- / sk-ant- / sk-proj- shapes
		if i+3 <= len(s) && s[i:i+3] == "sk-" {
			j := i + 3
			if j < len(s) && s[j] == '[' {
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
		// "api_key":"..." or "api-key":"..."
		if i+8 <= len(lower) && (lower[i:i+8] == `"api_key` || (i+9 <= len(lower) && lower[i:i+9] == `"api-key"`)) {
			// find next quoted value
			rest := s[i:]
			q1 := strings.Index(rest, `":"`)
			if q1 < 0 {
				q1 = strings.Index(rest, `": "`)
			}
			if q1 >= 0 {
				start := i + q1
				// find opening quote of value
				vq := strings.Index(s[start:], `"`)
				if vq >= 0 {
					vstart := start + vq + 1
					vend := strings.Index(s[vstart:], `"`)
					if vend > 0 {
						b.WriteString(s[i:vstart])
						b.WriteString("[REDACTED]")
						i = vstart + vend
						continue
					}
				}
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
