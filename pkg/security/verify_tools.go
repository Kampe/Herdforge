package security

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerificationExecutables maps a verification/test command string into absolute
// binaries that must be on the process-exec allowlist for workers that run
// the repository verification contract (e.g. "go test ./...", "make all").
func VerificationExecutables(testCommand string) []string {
	testCommand = strings.TrimSpace(testCommand)
	if testCommand == "" {
		testCommand = "go test ./..."
	}
	fields := strings.Fields(testCommand)
	if len(fields) == 0 {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(paths ...string) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	head := fields[0]
	switch head {
	case "go":
		add(resolveBin("go")...)
		add(resolveBin("gofmt")...)
	case "make":
		add(resolveBin("make")...)
		add(resolveBin("go")...)
		add(resolveBin("gofmt")...)
	case "npm", "pnpm", "yarn", "node":
		add(resolveBin(head)...)
		add(resolveBin("node")...)
	default:
		if filepath.IsAbs(head) {
			add(head)
		} else {
			add(resolveBin(head)...)
		}
	}
	for _, f := range fields[1:] {
		switch f {
		case "go", "gofmt", "make", "rg", "node", "pnpm", "npm", "git":
			add(resolveBin(f)...)
		}
	}
	return out
}

// DefaultHarnessAllowHosts is the production NetworkAllowHosts set for limited
// network: exact routed model/API hosts only. Loopback is NOT included as a
// CONNECT destination (prevents arbitrary localhost service access).
// The OS seatbelt permits only the broker listen port on 127.0.0.1.
func DefaultHarnessAllowHosts() []string {
	return []string{
		"api.openai.com", "api.anthropic.com",
		"generativelanguage.googleapis.com",
		"api.x.ai", "api.groq.com",
		"openrouter.ai", "api.openrouter.ai",
		"api.deepseek.com",
	}
}

func resolveBin(name string) []string {
	var out []string
	if p, err := exec.LookPath(name); err == nil {
		out = append(out, p)
	}
	for _, c := range []string{
		"/usr/bin/" + name,
		"/bin/" + name,
		"/usr/local/bin/" + name,
		"/opt/homebrew/bin/" + name,
	} {
		if _, err := os.Stat(c); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// ExecAllowlistForGrant builds the process-exec allowlist from AllowedTools
// plus verification profile binaries. Shell (bash/sh) is admitted ONLY when
// shell-exec is on the grant. Minimal probe helpers (cat, true, env) are always
// included so ProveDenials can run without granting interactive shell.
func ExecAllowlistForGrant(grant *LaunchGrant, agentBinary, testCommand string) []string {
	seen := map[string]struct{}{}
	add := func(paths ...string) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	// Minimal non-shell helpers for containment probes and env wrapper.
	add(
		"/usr/bin/env", "/usr/bin/sandbox-exec",
		"/usr/bin/true", "/bin/true",
		"/bin/cat", "/bin/ls",
		"/usr/bin/head", "/usr/bin/tail", "/usr/bin/grep",
		"/bin/echo", "/usr/bin/printf",
	)
	if agentBinary != "" {
		add(agentBinary)
	}
	if grant != nil {
		for _, tool := range grant.AllowedTools {
			add(toolExecPaths(tool)...)
		}
		if containsTool(grant.AllowedTools, "herd-verify") ||
			containsTool(grant.AllowedTools, "shell-exec") ||
			containsTool(grant.AllowedTools, "git-write") {
			add(VerificationExecutables(testCommand)...)
			add(resolveBin("rg")...)
		}
		if containsTool(grant.AllowedTools, "herd-verify-read") {
			add(VerificationExecutables(testCommand)...)
		}
	}
	// Reviewer: strip write/network-oriented binaries even if tool map slipped.
	strip := map[string]bool{}
	if grant != nil && strings.EqualFold(grant.Role, RoleReviewer) {
		for _, p := range []string{
			"/usr/bin/curl", "/usr/bin/ssh", "/usr/bin/scp",
			"/bin/rm", "/usr/bin/rm", "/bin/mv", "/usr/bin/mv",
		} {
			strip[p] = true
		}
		for p := range seen {
			base := filepath.Base(p)
			if base == "curl" || base == "ssh" || base == "scp" || base == "rm" || base == "mv" {
				strip[p] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		if strip[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}
