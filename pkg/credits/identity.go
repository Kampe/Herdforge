package credits

import (
	"os"
	"strings"
)

func ClaudeActiveEmail(accountsDir string) string {
	if accountsDir == "" {
		accountsDir = os.Getenv("HOME") + "/.claude/accounts"
	}
	sidecar := accountsDir + "/active.email"
	raw, err := os.ReadFile(sidecar)
	if err == nil {
		e := strings.TrimSpace(string(raw))
		if e != "" {
			return e
		}
	}
	return ""
}

func ClaudeEmailToAccount(email string) string {
	switch strings.ToLower(email) {
	case "blindside328@gmail.com", "blindside328":
		return "blindside328"
	case "nick.kampe@yugalabs.io", "yuga", "yugalabs":
		return "yuga"
	default:
		if idx := strings.Index(email, "@"); idx >= 0 {
			return email[:idx]
		}
		return email
	}
}

func ClaudeActiveAccount() string {
	email := ClaudeActiveEmail("")
	if email == "" {
		return ""
	}
	return ClaudeEmailToAccount(email)
}

func KnownAccountLookup(email string) string {
	emailL := email
	switch strings.ToLower(emailL) {
	case "blindside328@gmail.com", "blindside328":
		return "blindside328"
	case "nick.kampe@yugalabs.io", "yuga", "yugalabs":
		return "yuga"
	}
	return email
}

type CcUsageResult struct {
	BlocksJSON string
	DailyJSON  string
	Error      string
}
