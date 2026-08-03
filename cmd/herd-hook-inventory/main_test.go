package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harness"
)

func inventoryDiscovery(policies []harness.HookPolicy, revision string) harness.HookDiscovery {
	return harness.HookDiscoveryFunc(func(string) (harness.HookDiscoveryResult, error) {
		return harness.HookDiscoveryResult{State: harness.DiscoveryHooks, PolicyRequired: true, PolicyRevision: revision, Policies: policies, Hooks: []harness.Hook{{Name: "claude:pre-tool:" + strings.Repeat("a", 64), Requirement: harness.HookRequired}}}, nil
	})
}

func TestInventorySuccessIsJSONAndSecretSafe(t *testing.T) {
	var out, errOut bytes.Buffer
	digest := "claude:pre-tool:" + strings.Repeat("a", 64)
	d := inventoryDiscovery(nil, "sha256:"+strings.Repeat("b", 64))
	if code := run(nil, &out, &errOut, d); code != 0 || !strings.Contains(out.String(), `"handler_digest":"`+digest+`"`) || strings.Contains(out.String(), "http") || errOut.Len() != 0 {
		t.Fatalf("inventory code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestValidationSuccessAndPolicyFailuresAreNonVacuous(t *testing.T) {
	digest := "claude:pre-tool:" + strings.Repeat("a", 64)
	policy := harness.HookPolicy{HandlerDigest: digest, Requirement: harness.HookRequired, HealthURL: "http://127.0.0.1:8790/health"}
	revision := harness.HookPolicyRevision([]harness.HookPolicy{policy})
	var out, errOut bytes.Buffer
	if code := run([]string{"--validate"}, &out, &errOut, inventoryDiscovery([]harness.HookPolicy{policy}, revision)); code != 0 || !strings.Contains(out.String(), `"valid":true`) {
		t.Fatalf("validation success code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	for _, tc := range []struct {
		name, rev string
		policies  []harness.HookPolicy
	}{
		{"missing", revision, nil},
		{"stale", "sha256:" + strings.Repeat("c", 64), []harness.HookPolicy{policy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			errOut.Reset()
			if code := run([]string{"--validate"}, &out, &errOut, inventoryDiscovery(tc.policies, tc.rev)); code != 1 || errOut.Len() == 0 {
				t.Fatalf("failure code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestStrictArgumentsReturnUsageExit(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--unknown"}, &out, &errOut, inventoryDiscovery(nil, "")); code != 2 {
		t.Fatalf("unknown flag exit=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if code := run([]string{"claude"}, &out, &errOut, inventoryDiscovery(nil, "")); code != 2 {
		t.Fatalf("positional argument exit=%d", code)
	}
}

func TestInventoryRejectsUnknownBlankAndNonAuthoritativeDiscovery(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		result harness.HookDiscoveryResult
		err    error
	}{
		{name: "unknown-provider", args: []string{"--provider", "unknown"}},
		{name: "blank-provider", args: []string{"--provider", "  "}},
		{name: "not-discovered", result: harness.HookDiscoveryResult{State: harness.DiscoveryNotDiscovered}},
		{name: "failed-state", result: harness.HookDiscoveryResult{State: harness.DiscoveryFailed}},
		{name: "hooks-empty", result: harness.HookDiscoveryResult{State: harness.DiscoveryHooks}},
		{name: "no-hooks-with-handler", result: harness.HookDiscoveryResult{State: harness.DiscoveryNoHooks, Hooks: []harness.Hook{{Name: "claude:pre-tool:" + strings.Repeat("a", 64)}}}},
		{name: "discovery-error", err: errors.New("discovery failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			d := harness.HookDiscoveryFunc(func(string) (harness.HookDiscoveryResult, error) { return tc.result, tc.err })
			if code := run(tc.args, &out, &errOut, d); code != 1 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
		})
	}
}
