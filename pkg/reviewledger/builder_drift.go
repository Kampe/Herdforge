package reviewledger

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// LiveBuilder is the minimal, current identity evidence needed for the
// read-only standing builder-family drift diagnostic.
type LiveBuilder struct {
	Identity string
	Family   string
}

// BuilderFamilyDrift is one live standing builder whose family differs from
// the family recorded in its most recent independent review-ledger record.
type BuilderFamilyDrift struct {
	Lane     string
	Identity string
	Recorded string
	Live     string
}

// CompareStandingBuilderFamilies compares only declared standing lanes. It
// deliberately reads no policy and mutates no ledger state: this is a report
// about live-vs-recorded evidence, not review admission or provenance repair.
//
// A duplicate live identity or an unrecognized live/recorded family is
// ambiguous evidence and is refused rather than guessed. Lanes without both a
// live builder and a recorded builder family have no comparison to report.
func CompareStandingBuilderFamilies(cfg *config.Config, rows []LedgerRow, live []LiveBuilder) ([]BuilderFamilyDrift, error) {
	if cfg == nil {
		return nil, fmt.Errorf("builder-family drift: config is required")
	}
	liveByIdentity := make(map[string]LiveBuilder, len(live))
	for _, builder := range live {
		identity := strings.TrimSpace(builder.Identity)
		if identity == "" {
			return nil, fmt.Errorf("builder-family drift: live builder has empty identity")
		}
		if _, exists := liveByIdentity[identity]; exists {
			return nil, fmt.Errorf("builder-family drift: duplicate live builder identity %q", identity)
		}
		family := strings.TrimSpace(builder.Family)
		if !FamilyAllowlist[family] {
			return nil, fmt.Errorf("builder-family drift: live builder %q has unknown family %q", identity, family)
		}
		builder.Identity, builder.Family = identity, family
		liveByIdentity[identity] = builder
	}

	// JSONL order is append order, so later record rows supersede earlier
	// records for the same standing builder identity.
	recorded := make(map[string]string)
	for _, row := range rows {
		if row.Event != string(EventRecord) {
			continue
		}
		identity := strings.TrimSpace(row.BuilderIdentity)
		family := strings.TrimSpace(row.BuilderFamily)
		if identity == "" || family == "" {
			continue
		}
		if !FamilyAllowlist[family] {
			return nil, fmt.Errorf("builder-family drift: recorded builder %q has unknown family %q", identity, family)
		}
		recorded[identity] = family
	}

	findings := make([]BuilderFamilyDrift, 0)
	for _, lane := range standing.StandingLanes(cfg) {
		identity := standing.AgentName(lane.Name)
		recordedFamily, hasRecorded := recorded[identity]
		liveBuilder, hasLive := liveByIdentity[identity]
		if !hasRecorded || !hasLive || recordedFamily == liveBuilder.Family {
			continue
		}
		findings = append(findings, BuilderFamilyDrift{
			Lane: lane.Name, Identity: identity, Recorded: recordedFamily, Live: liveBuilder.Family,
		})
	}
	return findings, nil
}
