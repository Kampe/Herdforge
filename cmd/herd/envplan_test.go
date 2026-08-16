package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestParseDispatchArgsPreservesExactEnvironmentPlanID(t *testing.T) {
	req, err := parseDispatchArgs([]string{"FAC-311", "--environment-plan", "env-exact"})
	if err != nil {
		t.Fatal(err)
	}
	if req.EnvironmentPlanID != "env-exact" {
		t.Fatalf("environment plan id=%q, want exact selector", req.EnvironmentPlanID)
	}
}

func TestForgeDriverForwardsEnvironmentPlanToDispatch(t *testing.T) {
	var got []string
	restore := setHerdSubprocessForTest(func(args ...string) error { got = append([]string(nil), args...); return nil })
	t.Cleanup(restore)
	d := &cliForgeDriver{environmentPlanID: "env-exact"}
	if err := d.Dispatch(t.Context(), &provider.Task{Ref: "FAC-311"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range got {
		if got[i] == "--environment-plan" && i+1 < len(got) && got[i+1] == "env-exact" {
			found = true
		}
	}
	if !found {
		t.Fatalf("forge dispatch argv omitted exact plan: %v", got)
	}
}
