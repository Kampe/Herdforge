package launch

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
	"github.com/Kampe/Herdforge/pkg/router"
)

// Every provider the router can emit launch argv for must have a decided
// nested-agent posture: either a compiled vendor denial the boundary asserts,
// or an explicit declaration that no verified denial flag exists. A provider
// added to router.ArgvFor without one used to launch with neither.
func TestEveryRoutableProviderHasDecidedNestedAgentPosture(t *testing.T) {
	for _, provider := range []string{"claude", "agy", "codex", "grok", "kimi", "ollama", "opencode", "lazer"} {
		argv := router.ArgvFor(provider, "test-model", "medium")
		if len(argv) == 0 {
			t.Fatalf("provider %q emits no launch argv contract", provider)
		}
		compiled := agentpolicy.NestedDenyCompiled(provider)
		unguarded := agentpolicy.NestedDenyUnavailable(provider)
		if compiled == unguarded {
			t.Fatalf("provider %q must be exactly one of compiled/unguarded, got compiled=%v unguarded=%v",
				provider, compiled, unguarded)
		}
		if err := agentpolicy.RequireNestedDeny(provider, argv); err != nil {
			t.Fatalf("routable provider %q is refused at the launch boundary: %v", provider, err)
		}
	}
}

// Falsifiability for the check above: a provider nobody has classified must be
// refused rather than admitted by a default-allow branch. This fails against
// the pre-FAC-176 `default: return nil`.
func TestUnreviewedProviderIsRefusedAtLaunchBoundary(t *testing.T) {
	if err := agentpolicy.RequireNestedDeny("newvendor", []string{"newvendor", "--model", "m"}); err == nil {
		t.Fatal("a provider with no decided nested-agent posture must be refused, not silently launched")
	}
}

// The compiled providers must stay compiled: dropping one to the unguarded set
// would silently retire a denial the boundary currently asserts.
func TestCompiledProvidersRemainAsserted(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		if !agentpolicy.NestedDenyCompiled(provider) {
			t.Fatalf("provider %q must keep an asserted compiled denial", provider)
		}
		if err := agentpolicy.RequireNestedDeny(provider, []string{provider, "--model", "m"}); err == nil {
			t.Fatalf("provider %q must refuse argv lacking its compiled denial", provider)
		}
	}
}
