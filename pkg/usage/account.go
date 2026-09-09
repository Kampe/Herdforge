package usage

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/confinement"
)

// AccountIdentity binds a quota reading to the authenticated account it was
// fetched for, without carrying anything personal. Key is an opaque digest —
// it never contains a UUID, email, or token — and Provenance names exactly
// which local claim produced it, so a consumer can audit the binding.
//
// The key is derived from an ACCOUNT claim, never from a token: refreshing a
// token must not fork the billing account, and an access-token fingerprint
// would do exactly that. A missing or ambiguous claim leaves the identity nil
// (unknown), which downstream code must treat as unknown — never as healthy,
// never as exhausted, and never as a reason to merge two readings.
type AccountIdentity struct {
	Key        string `json:"key"`
	Provenance string `json:"provenance"`
}

// accountKeyDomain namespaces the digest so the same claim under a different
// provider, or a different scheme version, cannot collide.
const accountKeyDomain = "herd-account-v1"

func opaqueAccountKey(provider, claim string) string {
	sum := sha256.Sum256([]byte(accountKeyDomain + ":" + provider + ":" + claim))
	return hex.EncodeToString(sum[:])[:10]
}

func identity(provider, claim, provenance string) *AccountIdentity {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		return nil
	}
	return &AccountIdentity{
		Key:        opaqueAccountKey(provider, claim),
		Provenance: provenance,
	}
}

// providerAccountIdentity resolves the account identity for a native provider
// from local, authenticated credential/config claims only. No network, no
// keychain, no token use: these are file reads of claims the owning CLIs
// already wrote. Unknown providers and unprovable claims return nil.
func providerAccountIdentity(name string) *AccountIdentity {
	switch name {
	case "claude":
		// A cached Claude reading carries the authenticated /oauth/profile
		// claim. The CLI-maintained account UUID selects the current account and
		// rejects reuse after an account switch.
		return claudeConfigAccountIdentity()
	case "codex":
		return codexAccountIdentity()
	case "grok":
		return grokAccountIdentity()
	case "gemini":
		return geminiAccountIdentity()
	}
	return nil
}

// claudeAccountIdentity reads the account uuid Claude Code records in its
// config after OAuth login. The credentials file carries only token material,
// so this config claim — not the token — is what keeps a re-login's refresh
// from forking the account.
func claudeAccountIdentity() *AccountIdentity {
	return claudeConfigAccountIdentity()
}

func claudeConfigAccountIdentity() *AccountIdentity {
	for _, path := range claudeConfigFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			OauthAccount struct {
				AccountUUID string `json:"accountUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		if id := identity("claude", cfg.OauthAccount.AccountUUID, "claude-config:"+confinement.ClaudeTopLevelConfigFile+":oauthAccount.accountUuid"); id != nil {
			return id
		}
	}
	return nil
}

// claudeConfigAccountClaim is an expected-identity hint only. The native
// collector must compare it with the authenticated profile response before it
// exposes or caches a Claude reading.
func claudeConfigAccountClaim() string {
	for _, path := range claudeConfigFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			OauthAccount struct {
				AccountUUID      string `json:"accountUuid"`
				OrganizationUUID string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(raw, &cfg) != nil || strings.TrimSpace(cfg.OauthAccount.AccountUUID) == "" {
			continue
		}
		claim := strings.TrimSpace(cfg.OauthAccount.AccountUUID)
		if org := strings.TrimSpace(cfg.OauthAccount.OrganizationUUID); org != "" {
			claim += "|" + org
		}
		return claim
	}
	return ""
}

// claudeConfigFiles lists candidate .claude.json locations, most specific
// first, mirroring the credential file precedence.
func claudeConfigFiles() []string {
	var out []string
	add := func(dir string) {
		if strings.TrimSpace(dir) != "" {
			out = append(out, filepath.Join(dir, confinement.ClaudeTopLevelConfigFile))
		}
	}
	add(os.Getenv("CLAUDE_CONFIG_DIR"))
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		add(filepath.Join(x, "claude"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
		add(filepath.Join(home, ".config", "claude"))
	}
	return out
}

// codexAccountIdentity reads the account id the Codex CLI stores beside its
// OAuth tokens; it is stable across token refreshes.
func codexAccountIdentity() *AccountIdentity {
	for _, path := range codexCredentialFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var auth struct {
			Tokens struct {
				AccountID string `json:"account_id"`
			} `json:"tokens"`
		}
		if json.Unmarshal(raw, &auth) != nil {
			continue
		}
		if id := identity("codex", auth.Tokens.AccountID, "codex-auth:auth.json:tokens.account_id"); id != nil {
			return id
		}
	}
	return nil
}

// grokAccountIdentity uses the single auth.json entry key. More than one entry
// is ambiguous — which profile would the reading belong to? — and stays
// unknown rather than guessing.
func grokAccountIdentity() *AccountIdentity {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, ".grok", "auth.json"))
	if err != nil {
		return nil
	}
	var authMap map[string]json.RawMessage
	if json.Unmarshal(raw, &authMap) != nil {
		return nil
	}
	if len(authMap) != 1 {
		return nil
	}
	for key := range authMap {
		return identity("grok", key, "grok-auth:auth.json:entry-key")
	}
	return nil
}

// geminiAccountIdentity derives the key from the id_token `sub` claim in the
// Gemini CLI's oauth_creds.json. The JWT is decoded locally only to extract
// the stable account subject; it is never verified here (the credential file
// itself is the authenticated artifact) and neither the subject nor the email
// is ever printed — only their digest.
func geminiAccountIdentity() *AccountIdentity {
	for _, path := range geminiCredentialFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var creds struct {
			IDToken string `json:"id_token"`
		}
		if json.Unmarshal(raw, &creds) != nil {
			continue
		}
		claim := jwtSubject(creds.IDToken)
		if id := identity("gemini", claim, "gemini-oauth:oauth_creds.json:id_token.sub"); id != nil {
			return id
		}
	}
	return nil
}

// jwtSubject extracts the `sub` claim (falling back to `email`) from a JWT's
// payload without verification. Empty when the token is malformed or carries
// no subject.
func jwtSubject(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Sub != "" {
		return claims.Sub
	}
	return claims.Email
}
