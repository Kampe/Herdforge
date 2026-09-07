package main

import (
	"fmt"

	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// integrationEvidence keeps a native lifecycle visible after merge and ledger
// consumption remove it from unmerged discovery. It does not set HarvestReady
// or invent review evidence. Actual step admission still runs in the adapter.
func (a *drainAdapters) integrationEvidence(discovered []drainActionEvidence) ([]drainActionEvidence, error) {
	pending, err := integration.PendingCandidate(a.root)
	if err != nil || pending == "" {
		return discovered, err
	}
	rows, err := a.ledger.AllRows()
	if err != nil {
		return nil, err
	}
	resume := drainActionEvidence{SHA: pending, IntegrationPending: true}
	for _, row := range rows {
		if row.Event != string(reviewledger.EventRecord) || row.SHA != pending {
			continue
		}
		if row.Branch != "" {
			resume.Branch = row.Branch
		}
		if row.BuilderFamily != "" {
			resume.BuilderFamily = row.BuilderFamily
		}
		if row.Tier != "" {
			resume.Tier, resume.TierRecorded = row.Tier, true
		}
		if row.Lane != "" {
			resume.Lane = row.Lane
		}
	}
	if resume.Branch == "" || resume.BuilderFamily == "" || !resume.TierRecorded {
		return nil, fmt.Errorf("integration: pending candidate lacks recorded branch/family/tier")
	}
	out := []drainActionEvidence{resume}
	for _, e := range discovered {
		if e.SHA == pending {
			continue
		}
		// Reviews retain their own capacity policy. A second harvest may not
		// jump ahead of the active candidate's runtime/proof/cleanup stages.
		e.HarvestReady, e.IntegrationPending = false, false
		out = append(out, e)
	}
	return out, nil
}
