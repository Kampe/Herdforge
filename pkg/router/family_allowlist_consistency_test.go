package router_test

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
)

// FAC-595 repair: KnownFamilies must be a SET superset of both ledger builder
// allowlists. The ModelFor walk in family_validation_test.go never sees the
// lazer→proxy fallback; this assertion covers every family the ledger already
// treats as legitimate, including proxy.
func TestKnownFamiliesSupersetOfLedgerAllowlists(t *testing.T) {
	known := map[string]bool{}
	for _, f := range router.KnownFamilies() {
		known[f] = true
	}
	// Assert the allowlists actually contain proxy before checking coverage —
	// a silent no-op rename of the map key would otherwise look like success.
	if !review.LedgerFamilyAllowlist["proxy"] {
		t.Fatal("expected LedgerFamilyAllowlist to contain proxy; fixture drifted")
	}
	if !reviewledger.FamilyAllowlist["proxy"] {
		t.Fatal("expected reviewledger.FamilyAllowlist to contain proxy; fixture drifted")
	}
	for family := range review.LedgerFamilyAllowlist {
		if !known[family] {
			t.Fatalf("LedgerFamilyAllowlist has %q but router.KnownFamilies omits it; "+
				"ValidateFamily would refuse a recorded %s author", family, family)
		}
	}
	for family := range reviewledger.FamilyAllowlist {
		if !known[family] {
			t.Fatalf("reviewledger.FamilyAllowlist has %q but router.KnownFamilies omits it", family)
		}
	}
}
