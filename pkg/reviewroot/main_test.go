package reviewroot

import (
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

// FAC-610: TestNonRepoResolutionIsMarkedNonCanonical asserts that a
// cwd-relative fallback does NOT claim to be canonical. A fleet lane inherits
// HERD_ROOT and HERD_PROJECT_ROOT, which let the path resolve properly, so the
// test failed in every lane's shell and passed in the coordinator's -- same
// commit, same worktree, same machine.
func TestMain(m *testing.M) {
	laneenv.Strip()
	os.Exit(m.Run())
}
