package security

import (
	"fmt"
	"strings"
	"sync"
)

// FleetReadiness is the production fail-closed gate for FAC-133 harness readiness.
// Zero usable Claude/Codex/Grok harnesses MUST block dispatch/pulse/preflight.
type FleetReadiness struct {
	Usable  int
	Results []HarnessProbeResult
	Blocked bool
	Reason  string
}

// ErrFleetBlocked is returned when the fleet is not ready for write-capable work.
var ErrFleetBlocked = fmt.Errorf("%w: FAC-133 fleet harness readiness BLOCKED", ErrUnknownPolicy)

// readinessOverride allows tests to inject readiness without live harness probes.
var (
	readinessMu       sync.Mutex
	readinessOverride *FleetReadiness
	// skipReadinessGate disables the gate only under explicit test seam.
	skipReadinessGate bool
)

// SetReadinessOverrideForTest injects readiness for unit tests. Production never calls this.
func SetReadinessOverrideForTest(r *FleetReadiness) (restore func()) {
	readinessMu.Lock()
	prev := readinessOverride
	readinessOverride = r
	readinessMu.Unlock()
	return func() {
		readinessMu.Lock()
		readinessOverride = prev
		readinessMu.Unlock()
	}
}

// SkipReadinessGateForTest disables the production gate (unit tests only).
func SkipReadinessGateForTest(skip bool) (restore func()) {
	readinessMu.Lock()
	prev := skipReadinessGate
	skipReadinessGate = skip
	readinessMu.Unlock()
	return func() {
		readinessMu.Lock()
		skipReadinessGate = prev
		readinessMu.Unlock()
	}
}

// EvaluateFleetReadiness consumes durable attestation by default.
// Live Herdr model sessions are only spawned when HERD_LIVE_HARNESS_PROOF /
// HERD_REFRESH_READINESS is set (single-flight refresh). Pulse/dispatch never
// respawn proofs — they fail closed on missing/stale/revoked attestation.
func EvaluateFleetReadiness() (*FleetReadiness, error) {
	readinessMu.Lock()
	override := readinessOverride
	readinessMu.Unlock()
	if override != nil {
		cp := *override
		if cp.Blocked {
			return &cp, fmt.Errorf("%w: %s", ErrFleetBlocked, cp.Reason)
		}
		return &cp, nil
	}

	root := ResolveReadinessRoot()

	// Prefer durable attestation (no live spawn).
	if fr, err := ConsumeFleetAttestation(root); err == nil {
		return fr, nil
	}

	// Optional live refresh — single-flight; never implicit in production.
	if allowLiveHarnessRefresh() {
		return RefreshFleetAttestationLive(root)
	}

	// Honest BLOCKED without live spawn (do not call ProbeAllSupportedHarnesses).
	fr := &FleetReadiness{
		Blocked: true,
		Reason: "FAC-133 BLOCKED: no valid durable fleet attestation " +
			"(binary+policy+containment-bound). Refresh once with " +
			"HERD_LIVE_HARNESS_PROOF=1 herd preflight; pulse/dispatch only consume attestation.",
	}
	return fr, fmt.Errorf("%w: %s", ErrFleetBlocked, fr.Reason)
}

// RequireFleetReady is the production fail-closed gate. Call from preflight,
// pulse, dispatch launch. Consumes durable attestation only — never spawns
// live model sessions.
func RequireFleetReady() error {
	readinessMu.Lock()
	skip := skipReadinessGate
	readinessMu.Unlock()
	if skip {
		return nil
	}
	_, err := EvaluateFleetReadiness()
	return err
}

// FormatReadinessReport is human-readable for preflight/selftest output.
func FormatReadinessReport(fr *FleetReadiness) string {
	if fr == nil {
		return "FAC-133 readiness: (nil)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FAC-133 readiness: usable=%d blocked=%v\n", fr.Usable, fr.Blocked)
	if fr.Reason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", fr.Reason)
	}
	for _, r := range fr.Results {
		fmt.Fprintf(&b, "  %s: usable=%v tool=%v model=%v parent=%v via_launch_agent=%v contained=%v herdr=%v blocker=%q\n",
			r.Kind, r.Usable, r.ToolOK, r.ModelOK, r.PostParentAlive, r.ViaLaunchAgent, r.Contained, r.RealHerdrSession, r.TicketScopedBlocker)
	}
	return b.String()
}
