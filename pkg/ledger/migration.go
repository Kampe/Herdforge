package ledger

import (
	_ "embed"
	"strings"
)

//go:embed migrations/0001_phase1_ledger.sql
var phase1Migration string

// MigrationSQL returns the Phase 1 PostgreSQL migration. Applying it to a
// fresh database creates only Herdforge's logical schema; Cauldron remains a
// separately owned schema.
func MigrationSQL() string {
	return strings.TrimSpace(phase1Migration) + "\n"
}
