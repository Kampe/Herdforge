package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harness"
)

// pinWith writes a pin file containing exactly the given policies.
func pinWith(t *testing.T, dir string, policies []harness.HookPolicy) string {
	t.Helper()
	path := filepath.Join(dir, "harness-hooks.json")
	file := hooksPinFile{Providers: map[string]hooksPinProvider{
		"claude": {
			Hooks:               []harness.Hook{},
			ApprovedAuthorities: []string{"http://127.0.0.1:9999"},
			Policies:            policies,
		},
	}}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func readPin(t *testing.T, path string) hooksPinProvider {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f hooksPinFile
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return f.Providers["claude"]
}

// FAC-594: the pin file is the ONLY source of hook policies, and
// ApplyHookPolicies refuses any policy matching no live hook. A changed
// ~/.claude hook changes its digest, so the pin keeps naming a handler that no
// longer exists and one stale entry grounds every standing launch with
// hook.policy_mismatch. There was no command to repair it, which is what pushed
// agents onto raw `herdr agent start` and its silent low-tier model.
//
// The round-trip shape is asserted here; the live-discovery half is exercised by
// running the command against the real harness, which is how the two real
// orphans on this fleet were found.
func TestHooksPinRoundTripPreservesClassificationsAndAuthorities(t *testing.T) {
	dir := t.TempDir()
	kept := harness.HookPolicy{
		HandlerDigest: "claude:lifecycle:keepme",
		Requirement:   harness.HookRequired,
		HealthURL:     "http://127.0.0.1:9999/health",
	}
	orphan := harness.HookPolicy{
		HandlerDigest: "claude:lifecycle:orphan",
		Requirement:   harness.HookOptional,
	}
	path := pinWith(t, dir, []harness.HookPolicy{kept, orphan})

	got := readPin(t, path)
	if len(got.Policies) != 2 {
		t.Fatalf("fixture should hold 2 policies, got %d", len(got.Policies))
	}
	// An operator-set requirement and health URL must survive a refresh: a
	// refresh that silently downgrades a gate is worse than a stale pin.
	var found bool
	for _, p := range got.Policies {
		if p.HandlerDigest == kept.HandlerDigest {
			found = true
			if p.Requirement != harness.HookRequired || p.HealthURL == "" {
				t.Errorf("classification lost: %+v", p)
			}
		}
	}
	if !found {
		t.Error("kept policy missing from round trip")
	}
	if len(got.ApprovedAuthorities) != 1 {
		t.Errorf("approved_local_authorities must survive, got %v", got.ApprovedAuthorities)
	}
}

// The derived revision must be a pure function of the policy set, because
// FileDiscovery recomputes it and refuses a mismatch. If a refresh wrote a
// revision that disagreed with its own policies, discovery would fail outright.
func TestHookPolicyRevisionIsDerivedFromPolicies(t *testing.T) {
	a := []harness.HookPolicy{
		{HandlerDigest: "b", Requirement: harness.HookOptional},
		{HandlerDigest: "a", Requirement: harness.HookOptional},
	}
	b := []harness.HookPolicy{
		{HandlerDigest: "a", Requirement: harness.HookOptional},
		{HandlerDigest: "b", Requirement: harness.HookOptional},
	}
	if harness.HookPolicyRevision(a) != harness.HookPolicyRevision(b) {
		t.Error("revision must be order-independent, or a re-sort would look like drift")
	}
	c := append([]harness.HookPolicy{}, a...)
	c = append(c, harness.HookPolicy{HandlerDigest: "c", Requirement: harness.HookOptional})
	if harness.HookPolicyRevision(a) == harness.HookPolicyRevision(c) {
		t.Error("adding a policy must change the revision")
	}
}

// A policy set that does not bind must never be written. Writing one would
// leave the fleet grounded behind a file that looks freshly repaired, which is
// the exact failure this command exists to end.
func TestApplyHookPoliciesRefusesOrphanPolicy(t *testing.T) {
	live := []harness.Hook{{Name: "claude:lifecycle:live", Requirement: harness.HookOptional}}
	policies := []harness.HookPolicy{
		{HandlerDigest: "claude:lifecycle:live", Requirement: harness.HookOptional},
		{HandlerDigest: "claude:lifecycle:orphan", Requirement: harness.HookOptional},
	}
	_, code, digest := harness.ApplyHookPolicies(live, policies, harness.HookPolicyRevision(policies))
	if code == harness.HookCodeHealthy {
		t.Fatal("an orphan policy must not bind")
	}
	if digest != "claude:lifecycle:orphan" {
		t.Errorf("the offending digest should be named, got %q", digest)
	}

	// And the repaired set must bind, which is the pre-write check.
	fixed := policies[:1]
	if _, code, _ := harness.ApplyHookPolicies(live, fixed, harness.HookPolicyRevision(fixed)); code != harness.HookCodeHealthy {
		t.Errorf("dropping the orphan must make the set bind, got %s", code)
	}
}
