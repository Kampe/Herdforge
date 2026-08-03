package preflight

import (
	"os"
	"testing"
)

// TestMain isolates the cross-process reservation ledger per test run so
// fake-FSID entries never pollute (or read) the host's real ledger.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "herd-disk-ledger-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv(EnvDiskLedgerDir, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
