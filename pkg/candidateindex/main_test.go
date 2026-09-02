package candidateindex

import (
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

// TestMain keeps candidate-index fixtures independent from the worker pane
// that launches the test process. HERD_PROJECT_ROOT can make a fixture scan
// the shared checkout, while HERD_REVIEW_LEDGER can make it read the live
// review evidence; either inherited value turns isolated tests into a scan of
// fleet state. Tests that need either value must set it explicitly.
func TestMain(m *testing.M) {
	laneenv.Strip()
	restore, err := laneenv.IsolateDefaultSlotDir()
	if err != nil {
		os.Exit(1)
	}
	_ = os.Unsetenv("HERD_REVIEW_LEDGER")
	code := m.Run()
	restore()
	os.Exit(code)
}

// This is intentionally a direct guard: removing the package TestMain must
// make the test fail in a worker pane, rather than leaving the setup as an
// unverified convention.
func TestCandidateIndexTestMainStripsInheritedMetadata(t *testing.T) {
	for _, name := range []string{"HERD_ROOT", "HERD_PROJECT_ROOT", "HERD_REVIEW_LEDGER"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Fatalf("inherited fleet metadata %s=%q reached candidate-index tests", name, value)
		}
	}
}
