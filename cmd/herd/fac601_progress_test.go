package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/integration"
)

// Only the effect backend is fake. The drain adapter must read back the real
// on-disk transaction, not trust a mock's claim that a step completed.
type drainProgressFixture struct{}

func (drainProgressFixture) Check(context.Context, integration.Transaction, integration.Intent) error {
	return nil
}
func (drainProgressFixture) Observe(_ context.Context, _ integration.Transaction, in integration.Intent) (integration.Observation, error) {
	return integration.Observation{Intent: in, State: integration.EffectApplied, Evidence: "fixture observed effect"}, nil
}
func (drainProgressFixture) Execute(context.Context, integration.Transaction, integration.Intent) error {
	panic("already-applied fixture must not execute")
}

func TestFAC601DrainAcceptsOnlyDurableExactIntermediateProgress(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-sha", "wrong-evidence", "wrong-operation", "unrecorded-step", "errors", "refused-review", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			a := drainAdaptersWithRecord(t)
			t.Setenv(integration.StoreDirEnv, "")
			if out, err := testgit.Command(a.root, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			ctx := context.Background()
			if _, err := integration.Advance(ctx, a.root, drainSelftestSHA, integration.StepPass, drainProgressFixture{}); err != nil {
				t.Fatal(err)
			}
			r, err := integration.Advance(ctx, a.root, drainSelftestSHA, integration.StepHarvest, drainProgressFixture{})
			if err != nil {
				t.Fatal(err)
			}
			res := &harvest.IntegrationResult{Progress: r}
			switch mode {
			case "wrong-sha":
				r.Candidate = strings.Repeat("f", 40)
			case "wrong-evidence":
				r.Evidence = "not the observed effect"
			case "wrong-operation":
				r.OperationID = strings.Repeat("f", 32)
			case "unrecorded-step":
				r.Step = integration.StepMerge
			case "refused-review":
				res.ReviewGatedSHAs = []harvest.ReviewGateOutcome{{SHA: drainSelftestSHA, Eligible: false}}
			case "errors":
				res.Errors = []string{"effect readback failed"}
			}
			a.run = func(context.Context, string, harvest.AdmissionContext, bool) (*harvest.IntegrationResult, error) {
				return res, nil
			}
			err = a.integrate(ctx, drainSelftestEvidence(), mode == "dry-run")
			if mode == "valid" {
				if err != nil {
					t.Fatalf("durably completed harvest step required an immediate merge: %v", err)
				}
				if len(res.MergedSHAs) != 0 {
					t.Fatal("intermediate step invented merge evidence")
				}
			} else if err == nil {
				t.Fatal("unproven progress accepted")
			}
		})
	}
}
