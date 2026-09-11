package resolve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/posture"
)

// childEnvVar re-enters this test binary as the isolated fixture process. It is
// deliberately not HERDR_-prefixed: laneenv.Strip sweeps that prefix wholesale.
const childEnvVar = "HERD_TEST_INHERITED_POSTURE_CHILD"

// resolveOffClaude routes a lane that prefers ollama. Its clean answer is
// ollama; a claude-only posture rewrites it to a pinned claude.
func resolveOffClaude(t *testing.T) *ResolvedLane {
	t.Helper()
	reg := mustParseRegistry(t, testRegistryJSON)
	return New(reg, &mockScorer{providers: []string{"ollama"}}).Resolve("ux-comber", false)
}

// resolveOnClaude routes an unpinned lane whose only healthy provider is
// claude. Its clean answer is claude; a no-claude posture drops it.
func resolveOnClaude(t *testing.T) *ResolvedLane {
	t.Helper()
	reg := mustParseRegistry(t, testRegistryJSON)
	return New(reg, &mockScorer{providers: []string{"claude"}}).Resolve("platform-ops", false)
}

func offClaudeIsClean(r *ResolvedLane) bool { return r.Resolvable && r.Provider == "ollama" }
func onClaudeIsClean(r *ResolvedLane) bool  { return r.Resolvable && r.Provider == "claude" }

// writeHostPosture builds a durable posture directory of the shape an operator's
// $HOME carries: generation-fenced JSON plus the legacy sentinel mirror. Both
// are written because Effective falls back to the sentinel whenever the JSON is
// missing — isolating one and not the other still leaks.
func writeHostPosture(t *testing.T, mode posture.Mode, sentinel posture.Name) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_STATE_DIR", dir)
	a, err := posture.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), mode, "operator", "adversarial-host-state", "fleet", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(sentinel)), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestFixturesIgnoreInheritedHostPosture is the containment proof: a durable
// claude-only or no-claude posture inherited from the operator's machine must
// not reach a default fixture, while a posture a test sets deliberately must
// still drive the real implementation.
//
// It is a subprocess test because the leak is a property of process startup —
// TestMain's laneenv.Strip + laneenv.Isolate run before any test can observe
// them, so nothing in-process can prove they neutralised an inherited value.
func TestFixturesIgnoreInheritedHostPosture(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        posture.Mode
		sentinel    posture.Name
		env         string
		contaminate func(*testing.T) *ResolvedLane
		clean       func(*ResolvedLane) bool
	}{
		{"claude-only", posture.ModeClaudeOnly, posture.ClaudeOnly, "HERD_CLAUDE_ONLY", resolveOffClaude, offClaudeIsClean},
		{"no-claude", posture.ModeNoClaude, posture.NoClaude, "HERD_NO_CLAUDE", resolveOnClaude, onClaudeIsClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostDir := writeHostPosture(t, tc.mode, tc.sentinel)

			// Non-vacuity: the state we are about to hand the child really does
			// contaminate, through the production entry point, in this process.
			// Without this the child could pass because the fixture is inert.
			got, _, err := posture.Effective(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.mode {
				t.Fatalf("host state under test is inert: Effective=%q, want %q", got, tc.mode)
			}
			if res := tc.contaminate(t); tc.clean(res) {
				t.Fatalf("host %s did not change the fixture, so the child proves nothing: %+v", tc.name, res)
			}

			cmd := exec.Command(os.Args[0], "-test.run", "^TestInheritedPostureChildFixturesAreClean$", "-test.v")
			cmd.Env = append(os.Environ(),
				childEnvVar+"=1",
				"HERD_STATE_DIR="+hostDir,
				tc.env+"=1",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("isolated child inherited %s posture: %v\n%s", tc.name, err, out)
			}
		})
	}
}

// TestInheritedPostureChildFixturesAreClean runs only inside the subprocess the
// parent spawns with an adversarial posture in its environment and its durable
// state directory.
func TestInheritedPostureChildFixturesAreClean(t *testing.T) {
	if os.Getenv(childEnvVar) == "" {
		t.Skip("parent role: spawned by TestFixturesIgnoreInheritedHostPosture")
	}

	mode, _, err := posture.Effective(context.Background())
	if err != nil {
		t.Fatalf("isolated fixture posture failed closed: %v", err)
	}
	if mode != posture.ModeClear {
		t.Fatalf("inherited posture reached the fixture: Effective=%q, want clear", mode)
	}
	// A claude-only leak breaks the first; a no-claude leak breaks the second.
	if res := resolveOffClaude(t); !offClaudeIsClean(res) {
		t.Fatalf("off-Claude fixture contaminated: resolvable=%v provider=%q reason=%s", res.Resolvable, res.Provider, res.Reason)
	}
	if res := resolveOnClaude(t); !onClaudeIsClean(res) {
		t.Fatalf("on-Claude fixture contaminated: resolvable=%v provider=%q reason=%s", res.Resolvable, res.Provider, res.Reason)
	}

	// Deliberate posture must still reach the real implementation from inside
	// the isolated process — isolation is a clean slate, not a bypass.
	t.Setenv("HERD_CLAUDE_ONLY", "1")
	res := resolveOffClaude(t)
	if offClaudeIsClean(res) {
		t.Fatalf("deliberate claude-only was neutralised by test isolation: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Constraints, " ")+res.Reason, "claude-only") {
		t.Fatalf("deliberate claude-only refusal lost its rationale: %+v", res)
	}
}
