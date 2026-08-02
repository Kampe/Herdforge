package router

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestRouteParityAgainstChainseer runs the ORIGINAL zsh bin/herd-route in
// doctor mode for every shape and asserts this port resolves the identical
// provider→model and family tables. Skipped unless CHAINSEER_ROOT points at a
// chainseer checkout — on the operator's machine this is the live parity
// contract for the shell→Go swap. Verified PASS 2026-08-02 (9 shapes × 8
// providers, 187s of live probing).
func TestRouteParityAgainstChainseer(t *testing.T) {
	root := os.Getenv("CHAINSEER_ROOT")
	if root == "" {
		t.Skip("set CHAINSEER_ROOT to run shell parity check")
	}
	script := filepath.Join(root, "bin", "herd-route")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("no herd-route at %s", script)
	}

	shapes := []string{
		"coordinator", "architecture", "implementation", "research",
		"bounded", "advisory", "qa-light", "qa", "adversarial",
	}
	for _, shape := range shapes {
		cmd := exec.Command(script, "--doctor", "--task", shape, "--json")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("zsh herd-route --doctor --task %s: %v", shape, err)
		}
		var rows []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Family   string `json:"family"`
		}
		if err := json.Unmarshal(out, &rows); err != nil {
			t.Fatalf("parse doctor output for %s: %v", shape, err)
		}
		for _, row := range rows {
			if row.Provider == "graph" {
				continue // doctor appends a tooling row, not a routing surface
			}
			if got := ModelFor(row.Provider, shape); got != row.Model {
				t.Errorf("shape=%s provider=%s: Go model %q != zsh model %q",
					shape, row.Provider, got, row.Model)
			}
			if got := FamilyFor(row.Provider, row.Model); got != row.Family {
				t.Errorf("shape=%s provider=%s: Go family %q != zsh family %q",
					shape, row.Provider, got, row.Family)
			}
		}
	}
}
