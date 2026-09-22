package main

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// reviewVerificationBudgetSection is the native-review verification policy.
// Mandatory checks stay: inspect the exact diff, run targeted tests, file a
// verdict. Full unit suites remain the hosted CI contract
// (verification.test_command / make test-unit / required collector job
// "Build, Preflight & Test Suite"). Native review serializes and budgets;
// it does not fan out overlapping suites.
func reviewVerificationBudgetSection(scopedTestCmd string) string {
	scoped := strings.TrimSpace(scopedTestCmd)
	if scoped == "" {
		scoped = "go test -count=1 ./<changed-packages>/"
	}
	return fmt.Sprintf(`VERIFICATION BUDGET — serialize, do not fan out
Launch env is GOMAXPROCS=%s and GOFLAGS includes %s (other GOFLAGS are kept). Do not raise GOMAXPROCS or go -p.

Mandatory checks, one process at a time, in this order:
1. Inspect only git diff origin/main..HEAD (or the packet base..HEAD).
2. Targeted tests: %s
3. Never start make test-unit, go test ./cmd/herd, and go test ./... together. They overlap. Concurrent redundant suites drove native review CPU admission to REFUSE.

Hosted CI owns the full unit suite (configured verification.test_command / make test-unit) and required collector job "Build, Preflight & Test Suite". If those passed for this exact SHA, cite the run. That is honest reuse. Do not re-run the full suite locally during native review unless targeted checks are inconclusive, and then only after they finish.

AGENTS.md package-grid commands are builder/CI instructions, not a native-review fan-out list.
`, herdr.ReviewBudgetGOMAXPROCS, herdr.ReviewBudgetGOFLAGS, scoped)
}
