package credits

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

func ClaudeActiveEmail(accountsDir string) string {
	if accountsDir == "" {
		accountsDir = os.Getenv("HERD_CLAUDE_ACCOUNTS_DIR")
	}
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
			return strings.ToLower(email[:idx])
		}
		return strings.ToLower(email)
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

var claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
	return exec.Command("claude", args...)
}

func ClaudeActiveExpanded() string {
	// try JSON output first
	cmd := claudeAuthStatusCmd("auth", "status", "--json")
	out, err := cmd.Output()
	if err == nil {
		var result struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(out, &result); err == nil && result.Email != "" {
			return result.Email
		}
	}

	// fallback: parse Email: line from text output
	cmd = claudeAuthStatusCmd("auth", "status", "--text")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Email:") {
			e := strings.TrimSpace(strings.TrimPrefix(line, "Email:"))
			if e != "" {
				return e
			}
		}
	}
	return ""
}

type CcUsageResult struct {
	BlocksJSON string
	DailyJSON  string
	Error      string
}
