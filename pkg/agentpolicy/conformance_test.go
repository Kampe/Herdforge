package agentpolicy

import (
	"errors"
	"path/filepath"
	"testing"
)

// Hermetic end-to-end conformance for FAC-173 acceptance:
// every fleet role, Herdforge + one external fixture repo, nested deny,
// shell/Herdr allow, missing contract fail-closed, recovery generation bind.

func TestConformanceRolesAndRepositories(t *testing.T) {
	key := []byte("conformance-key")
	roles := []string{"worker", "forge-smith", "reviewer", "verification-gate", "recovery", "coordinator", "recovery-sentinel"}
	repos := []string{
		"github.com/Kampe/Herdforge",
		"github.com/external/no-agents-file", // external fixture: no Herdforge AGENTS.md
	}
	children := []ChildKind{
		ChildClaudeAgent, ChildClaudeTask, ChildCodexSubagent, ChildCodexCollaboration,
		ChildRecovery, ChildReviewer, ChildVerifier, ChildWorker, ChildCoordinator, ChildExternalRepository,
	}
	path := filepath.Join(t.TempDir(), "denials.jsonl")
	store, err := NewEvidenceStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, role := range roles {
		for _, repo := range repos {
			b, c, err := BindLaunch(repo, "FAC-173", "lane-"+role, role, 4, "sess-"+role, "tab-"+role, "pane-"+role, "codex", SurfaceHerdrDispatch, key)
			if err != nil {
				t.Fatalf("%s/%s bind: %v", role, repo, err)
			}
			if err := RequireLaunchBinding(b, key, 4); err != nil {
				t.Fatalf("%s/%s require: %v", role, repo, err)
			}
			// Recovery after generation fence: exact generation only.
			if err := RequireLaunchBinding(b, key, 5); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("%s/%s recovery generation drift: %v", role, repo, err)
			}
			for _, child := range children {
				if _, err := Enforce(c, key, Attempt{
					Operation:  OperationNestedAgent,
					Child:      child,
					Repository: repo,
					Surface:    SurfaceNestedAgent,
					Family:     "codex",
				}, store); err != nil {
					t.Fatalf("%s/%s child %s: %v", role, repo, child, err)
				}
			}
			// Shell test processes remain allowed.
			if _, err := Enforce(c, key, Attempt{
				Operation: OperationShell, Repository: repo, Surface: SurfaceShell, Family: "codex",
			}, store); err != nil {
				t.Fatalf("%s/%s shell: %v", role, repo, err)
			}
			// Explicit Herdr fleet dispatch remains allowed.
			if _, err := Enforce(c, key, Attempt{
				Operation: OperationHerdrDispatch, Repository: repo, Surface: SurfaceHerdrDispatch, Family: "codex",
			}, store); err != nil {
				t.Fatalf("%s/%s herdr: %v", role, repo, err)
			}
		}
	}
}

func TestConformanceMissingContractFailsBeforeMutation(t *testing.T) {
	key := []byte("conformance-key")
	// Absent binding.
	if err := RequireLaunchBinding(LaunchBinding{}, key, 1); !errors.Is(err, ErrMissingBinding) {
		t.Fatalf("absent: %v", err)
	}
	// Unenforceable (wrong key).
	b, _, err := BindLaunch("repo", "task", "lane", "role", 1, "s", "t", "p", "codex", SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunchBinding(b, []byte("wrong-key"), 1); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestMutationRemovingNestedDenyIsCaught(t *testing.T) {
	// A test that cannot fail is a finding. Mutate a compiled argv by
	// stripping multi_agent denials and prove RequireNestedDeny fails.
	compiled, err := CompileCodexArgs([]string{"codex", "--model", "m", "-a", "never"})
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireNestedDeny("codex", compiled); err != nil {
		t.Fatal(err)
	}
	// Mutation: drop both --disable pairs (indices 1..4).
	mutated := append([]string{compiled[0]}, compiled[5:]...)
	if err := RequireNestedDeny("codex", mutated); err == nil {
		t.Fatal("removing nested-agent boundary must fail RequireNestedDeny")
	}
	// Claude mutation: strip disallowed-tools.
	cCompiled, err := CompileClaudeArgs([]string{"claude", "--model", "m", "--effort", "high"})
	if err != nil {
		t.Fatal(err)
	}
	// Drop denial block (indices 1..8 inclusive of Agent/Task/ToolSearch).
	cMut := append([]string{cCompiled[0]}, cCompiled[9:]...)
	if err := RequireNestedDeny("claude", cMut); err == nil {
		t.Fatal("removing claude nested-agent boundary must fail")
	}
}
