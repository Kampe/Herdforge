package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FAC-563: the fence broker is required infrastructure for any fenced board
// write, and nothing said so before the write failed.
//
// It is classed internal in the control surface so it does not appear in
// ordinary help, it was absent from the README and prompts, and no readiness
// gate checked for it. A restored checkout therefore has .herd/claim/fences.db
// and no broker, and the first symptom is a failed close in the middle of a
// mutation — the worst possible moment to learn about a dependency.

// FenceBrokerReadiness reports whether a fenced board write could succeed here.
type FenceBrokerReadiness struct {
	// Ready is true when a fenced write has a path to completion.
	Ready bool
	// Detail explains the current posture in one line.
	Detail string
	// Remedy is the operator action when not ready. Empty when ready.
	Remedy string
	// ClaimDir is the claim volume the verdict was computed against.
	ClaimDir string
}

// CheckFenceBroker reports fence-broker readiness WITHOUT failing.
//
// It is a report rather than a gate on purpose: most preflight runs are not
// about to perform a fenced board write, and failing them all would train
// operators to ignore the check. What matters is that the requirement is stated
// BEFORE work depends on it, with the command to satisfy it.
func CheckFenceBroker(claimDir string) FenceBrokerReadiness {
	r := FenceBrokerReadiness{ClaimDir: claimDir}

	// A coordinator that hosts the broker in its own process needs no external
	// one: the address space is the mint authority boundary (FAC-564). This is
	// the recorded intended contract for a coordinator.
	if strings.TrimSpace(os.Getenv("HERD_FENCE_COORDINATOR")) == "1" {
		r.Ready = true
		r.Detail = "coordinator hosts the fence broker in-process (HERD_FENCE_COORDINATOR=1); no external broker required"
		return r
	}
	if url := strings.TrimSpace(os.Getenv("HERD_FENCE_BROKER_URL")); url != "" {
		r.Ready = true
		r.Detail = "fence broker configured at " + url
		return r
	}
	if strings.TrimSpace(os.Getenv("HERD_FENCE_ATOMIC_SERVER")) == "1" {
		r.Ready = true
		r.Detail = "upstream board declares native fence+op-dedupe (HERD_FENCE_ATOMIC_SERVER=1); no broker required"
		return r
	}

	r.Detail = "no fence broker: fenced board writes WILL fail closed"
	if claimDir != "" {
		if _, err := os.Stat(filepath.Join(claimDir, "fences.db")); err == nil {
			// This is the exact trap: fence state present, no broker.
			r.Detail = fmt.Sprintf(
				"no fence broker, but fence state already exists at %s: fenced board writes WILL fail closed",
				filepath.Join(claimDir, "fences.db"))
		}
	}
	r.Remedy = strings.Join([]string{
		"A fenced board write needs one of:",
		"  1. Coordinator hosts it in-process (recommended for a coordinator):",
		"       export HERD_FENCE_COORDINATOR=1",
		"     Mint authority is then the process address space; do not also run a standalone broker,",
		"     because one broker per claim volume is enforced by the claim-dir lock.",
		"  2. A standalone broker, with this process as its client:",
		"       herd fence-broker --claim-dir <claim-dir>",
		"       export HERD_FENCE_BROKER_URL=<printed url>",
		"       export HERD_FENCE_BROKER_TOKEN=<printed worker token>",
		"     A worker holds only the worker token and can never mint.",
		"  3. An upstream board that natively enforces fence+op-dedupe:",
		"       export HERD_FENCE_ATOMIC_SERVER=1",
		"     Stock Kaneo does NOT qualify.",
	}, "\n")
	return r
}
