package ledger

import (
	"strings"
	"testing"
)

func TestPhaseOneContractsAreVersioned(t *testing.T) {
	contracts := []interface{ ContractVersion() int }{
		Actor{}, Principal{}, Run{}, Phase{}, Candidate{}, Receipt{}, Review{}, Approval{}, SpendEntry{}, OwnedWorktree{}, LifecycleEvent{},
	}
	for _, contract := range contracts {
		if got := contract.ContractVersion(); got != Version1 {
			t.Fatalf("contract version = %d, want %d", got, Version1)
		}
	}
}

func TestMigrationSeparatesHerdforgeFromCauldronAndProtectsCandidateEvidence(t *testing.T) {
	sql := MigrationSQL()
	for _, want := range []string{
		"CREATE SCHEMA IF NOT EXISTS herdforge",
		"CREATE TABLE IF NOT EXISTS herdforge.candidates",
		"CREATE TRIGGER candidates_identity_immutable",
		"CREATE TRIGGER lifecycle_events_append_only",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(sql), "create schema if not exists cauldron") {
		t.Fatal("Herdforge migration must not create or own Cauldron's logical schema")
	}
}
