package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runRoleInject ports bin/herd-role-inject: a SessionStart hook that binds a
// launched lane to its role/worker contract deterministically.
//
// When a launcher exports HERD_INJECT_FILES (colon-separated paths), emit a
// compact mandatory-read block naming them. Interactive sessions (env unset)
// are a silent no-op.
//
// It deliberately does NOT print file bodies. SessionStart hook output is
// truncated by the harness (~2KB observed); chainseer caught a ~33KB
// orchestrator.md silently truncating, with the "read it if absent" self-heal
// never firing. A short path list survives any cap, and the reads themselves
// are one tool call each and prompt-cache friendly.
func runRoleInject() {
	raw := strings.TrimSpace(os.Getenv("HERD_INJECT_FILES"))
	if raw == "" {
		return
	}

	fmt.Println("=== HERD LANE BINDING (herd role-inject) ===")
	fmt.Println("MANDATORY FIRST ACTION: Read each file below IN FULL, in order, before any other work. They are your worker contract and role; nothing else substitutes.")

	var listed []string
	for _, f := range strings.Split(raw, ":") {
		if f = strings.TrimSpace(f); f != "" {
			listed = append(listed, f)
		}
	}

	// AGENTS.md binds every injected session: governance and invariants outrank
	// a role prompt on posture questions. Prepend unless already listed.
	// Repo-relative, never absolute (CLAUDE.md invariant 3).
	governance := filepath.Join(".", "AGENTS.md")
	if _, err := os.Stat(governance); err == nil && !containsPath(listed, governance) {
		listed = append([]string{governance}, listed...)
	}

	for i, f := range listed {
		if _, err := os.Stat(f); err == nil {
			fmt.Printf("  %d. %s\n", i+1, f)
		} else {
			fmt.Printf("  %d. %s  (MISSING at launch: report this in your first handoff)\n", i+1, f)
		}
	}
	fmt.Println("=== END HERD LANE BINDING ===")
}

// containsPath compares by cleaned path so "./AGENTS.md" and "AGENTS.md" are
// recognised as the same entry and the contract is not listed twice.
func containsPath(list []string, want string) bool {
	want = filepath.Clean(want)
	for _, v := range list {
		if filepath.Clean(v) == want {
			return true
		}
	}
	return false
}
