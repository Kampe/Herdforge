// Package laneenv strips fleet launch metadata from a test process.
//
// FAC-610: a standing lane is launched with HERD_ROOT, HERD_PROJECT_ROOT,
// HERD_WORKSPACE, HERD_ROLE and HERDR_* pane variables set. Those inherit into
// `go test`, and tests that assert a DEGRADED or non-canonical resolution get a
// canonical one instead -- so they fail in a lane's shell and pass in a
// coordinator's, on the same commit, in the same worktree, on the same machine.
//
// Measured on 8811e89: clean env exit 0; with lane metadata set, six failures
// across cmd/herd and pkg/reviewroot. TestNonRepoResolutionIsMarkedNonCanonical
// is the clearest case -- it fails with "a cwd-relative fallback must not claim
// to be canonical" precisely BECAUSE HERD_ROOT let the path resolve properly.
//
// This is not a nuisance. The coordinator published a baseline measured in its
// own shell and told every lane that any other failure was genuinely theirs.
// That instruction was sound where it was measured and false in every lane, so
// each lane was directed to chase failures it did not cause. A signal that
// cannot be told apart from a real one is the defect family this whole control
// plane keeps paying for.
//
// The fix belongs in the suite, not in a rule lanes must remember. cmd/herd's
// TestMain already unset HERD_ROLE for exactly this reason; this generalises
// that precedent rather than inventing a second convention for one rule.
package laneenv

import (
	"os"
	"strings"
)

// Vars are the launch-metadata variables a fleet lane inherits. HERD_ROLE was
// already handled individually in cmd/herd; it belongs to the same class.
var Vars = []string{
	"HERD_ROOT",
	"HERD_PROJECT_ROOT",
	"HERD_WORKSPACE",
	"HERD_ROLE",
	"HERD_LANE",
}

// Prefixes are cleared wholesale: HERDR_* is pane metadata injected by the
// terminal layer, and enumerating it would go stale the moment herdr adds one.
var Prefixes = []string{"HERDR_"}

// Strip removes fleet launch metadata from this process.
//
// Call it from TestMain BEFORE m.Run(). It deliberately does NOT restore the
// values afterwards: the test binary is the whole process, nothing downstream
// wants them back, and restoring would reintroduce the leak for anything that
// re-reads the environment late.
//
// Tests that need one of these set must set it explicitly with t.Setenv, which
// is the point -- an intentional value is visible in the test, an inherited one
// is invisible everywhere.
func Strip() {
	for _, v := range Vars {
		_ = os.Unsetenv(v)
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, p := range Prefixes {
			if strings.HasPrefix(name, p) {
				_ = os.Unsetenv(name)
				break
			}
		}
	}
}

// Leaked reports which launch-metadata variables are still set. A suite can
// assert on this to prove Strip actually ran, rather than trusting that it did.
func Leaked() []string {
	var found []string
	for _, v := range Vars {
		if _, ok := os.LookupEnv(v); ok {
			found = append(found, v)
		}
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, p := range Prefixes {
			if strings.HasPrefix(name, p) {
				found = append(found, name)
				break
			}
		}
	}
	return found
}
