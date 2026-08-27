package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stuckProbe simulates a provider that never answers, without touching a CLI.
// The seam is CHA-2451's execProviderProbe, already in production for exactly
// this purpose.
func stuckProbe(t *testing.T) {
	t.Helper()
	original := execProviderProbe
	execProviderProbe = func(context.Context, string, []string, string) (string, string, error, bool) {
		return "", "", errors.New("signal: killed"), true
	}
	t.Cleanup(func() { execProviderProbe = original })
}

// FAC-596: a deadline expiry on the ADMISSION path returned the bare marker
// "provider probe timeout", and callers surface the reason verbatim as the
// routing reason. Three costs, all paid during the 2026-08-27T08:08Z incident:
//
//   - it named neither the provider/model being waited on nor how long it
//     waited, so a stalled admission queue could not be traced to a target;
//   - it read exactly like "quota" or "not logged in", a definitive refusal,
//     so a healthy-but-slow provider was routed away from on evidence that
//     never said it refused;
//   - the 45s budget was an inline literal no caller or test could name.
//
// Unknown is not a refusal.
func TestAdmissionProbeTimeoutNamesTargetAndElapsedAndReadsAsUnknown(t *testing.T) {
	stuckProbe(t)
	ok, reason := runProviderProbe("codex", "gpt-5.3-codex-spark", providerProbeBudget)
	if ok {
		t.Fatal("a stuck provider was admitted")
	}
	if !strings.Contains(reason, ProbeUnknownPrefix) {
		t.Fatalf("timeout does not read as unknown: %q", reason)
	}
	for _, want := range []string{"codex", "gpt-5.3-codex-spark", "45s"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("timeout reason does not name %q: %q", want, reason)
		}
	}
}

// CHA-2451's read paths must keep their terse machine marker. This change must
// not redecorate a token another landed change matches on.
func TestReadPathDeadlineMarkerIsPreserved(t *testing.T) {
	stuckProbe(t)
	ok, reason := runProviderProbe("codex", "gpt-5.3-codex-spark", AdvertisingProbeTimeout)
	if ok {
		t.Fatal("a stuck provider was admitted on the read path")
	}
	if reason != "provider_probe_deadline" {
		t.Fatalf("read-path deadline marker changed to %q; CHA-2451 consumers match on it", reason)
	}
}

// A real refusal must NOT be laundered into an unknown: quota exhaustion is a
// definitive no and routing away from it is correct.
func TestDefiniteRefusalIsNotReportedAsUnknown(t *testing.T) {
	_, reason := classifyProviderProbeOutput("", "quota exhausted", nil, false)
	if strings.Contains(reason, ProbeUnknownPrefix) {
		t.Fatalf("a definitive refusal was laundered into an unknown: %q", reason)
	}
}

// The deadline must be explicit and bounded, not an inline literal.
func TestProbeBudgetIsExplicitAndBounded(t *testing.T) {
	if providerProbeBudget <= 0 || providerProbeBudget > time.Minute {
		t.Fatalf("probe budget is not an explicit bounded deadline: %v", providerProbeBudget)
	}
}
